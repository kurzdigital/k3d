#!/bin/bash

# Test `cluster reconfigure -c <config>`: config-diff driven rolling node
# replacement. Covers: dry-run, agent-only change (no --force needed),
# server change (--force on single-server), idempotency (second apply is a
# no-op) and revocation (reverting to the original config removes the
# added volume/arg/env again). Plus a realistic single-server payload:
# an audit-policy file mounted via volume with the matching
# --kube-apiserver-arg.

CURR_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
[ -d "$CURR_DIR" ] || { echo "FATAL: no current dir (maybe running in zsh?)"; exit 1; }
source "$CURR_DIR/common.sh"

### Step Setup ###
LOG_FILE="$TEST_OUTPUT_DIR/$( basename "${BASH_SOURCE[0]}" ).log"
exec >${LOG_FILE} 2>&1
export LOG_FILE
KUBECONFIG="$KUBECONFIG_ROOT/$( basename "${BASH_SOURCE[0]}" ).yaml"
export KUBECONFIG
### Step Setup ###

export CURRENT_STAGE="Test | reconfigure-spec"

clustername="reconfspec"
soloclustername="reconfspecsolo"
CTX="k3d-$clustername"

ASSET_DIR="$TEST_OUTPUT_DIR/reconfigure-spec-assets"
mkdir -p "$ASSET_DIR/data"

cleanup() {
  $EXE cluster delete "$clustername" || true
  $EXE cluster delete "$soloclustername" || true
}
trap cleanup EXIT

# --- Config A: plain cluster, 1 server + 2 agents ---
cat > "$ASSET_DIR/config-a.yaml" <<EOF
apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: $clustername
servers: 1
agents: 2
EOF

# --- Config B: adds a volume + env on agents and a kube-apiserver-arg on the server ---
cat > "$ASSET_DIR/config-b.yaml" <<EOF
apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: $clustername
servers: 1
agents: 2
volumes:
  - volume: $ASSET_DIR/data:/mnt/reconftest
    nodeFilters:
      - agent:*
env:
  - envVar: RECONF_TEST=fromconfig
    nodeFilters:
      - agent:*
options:
  k3s:
    extraArgs:
      - arg: --kube-apiserver-arg=request-timeout=2m30s
        nodeFilters:
          - server:*
EOF

# --- Config B-agents: only the agent-scoped part of B (no server change) ---
cat > "$ASSET_DIR/config-b-agents.yaml" <<EOF
apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: $clustername
servers: 1
agents: 2
volumes:
  - volume: $ASSET_DIR/data:/mnt/reconftest
    nodeFilters:
      - agent:*
env:
  - envVar: RECONF_TEST=fromconfig
    nodeFilters:
      - agent:*
EOF

info "Creating cluster $clustername from config A ..."
$EXE cluster create "$clustername" --config "$ASSET_DIR/config-a.yaml" --timeout 360s \
  || failed "could not create cluster $clustername"

check_clusters "$clustername" || failed "cluster $clustername not ready"

server="k3d-$clustername-server-0"
server_id_before=$(docker inspect -f '{{.Id}}' "$server")

# --- 1. dry-run: prints diff, changes nothing ---
info "Dry-run against config B ..."
$EXE cluster reconfigure "$clustername" -c "$ASSET_DIR/config-b.yaml" --dry-run \
  || failed "dry-run failed"

for n in "$server" "k3d-$clustername-agent-0" "k3d-$clustername-agent-1"; do
  if docker inspect -f '{{range .Mounts}}{{.Destination}} {{end}}' "$n" | grep -q "/mnt/reconftest"; then
    failed "dry-run mutated node $n (mount appeared)"
  fi
done
[ "$(docker inspect -f '{{.Id}}' "$server")" = "$server_id_before" ] \
  || failed "dry-run replaced the server container"
passed "dry-run printed the diff without changing anything"

# --- 2. agent-only change: must work WITHOUT --force ---
info "Applying agent-only config (no --force) ..."
$EXE cluster reconfigure "$clustername" -c "$ASSET_DIR/config-b-agents.yaml" --ready-timeout 300s \
  || failed "agent-only reconfigure without --force failed"

for n in "k3d-$clustername-agent-0" "k3d-$clustername-agent-1"; do
  docker inspect -f '{{range .Mounts}}{{.Destination}} {{end}}' "$n" | grep -q "/mnt/reconftest" \
    || failed "volume mount missing on $n after reconfigure"
  docker inspect -f '{{range .Config.Env}}{{.}} {{end}}' "$n" | grep -q "RECONF_TEST=fromconfig" \
    || failed "env var missing on $n after reconfigure"
done
[ "$(docker inspect -f '{{.Id}}' "$server")" = "$server_id_before" ] \
  || failed "agent-only reconfigure replaced the server container"
check_clusters "$clustername" || failed "cluster unhealthy after agent-only reconfigure"
passed "agent-only reconfigure applied volume+env and left the server alone"

# --- 3. full config B: server change requires --force on single-server ---
info "Applying full config B without --force (must fail) ..."
if $EXE cluster reconfigure "$clustername" -c "$ASSET_DIR/config-b.yaml"; then
  failed "server-affecting reconfigure on single-server cluster succeeded without --force"
fi
passed "server-affecting reconfigure without --force correctly refused"

info "Applying full config B with --force ..."
$EXE cluster reconfigure "$clustername" -c "$ASSET_DIR/config-b.yaml" --force --ready-timeout 300s \
  || failed "full reconfigure with --force failed"

docker inspect -f '{{range .Config.Cmd}}{{.}} {{end}}' "$server" | grep -q -- "--kube-apiserver-arg=request-timeout=2m30s" \
  || failed "kube-apiserver-arg missing on server after reconfigure"
check_clusters "$clustername" || failed "cluster unhealthy after full reconfigure"
passed "full reconfigure applied the server arg"

# --- 4. idempotency: second apply must be all no-op ---
info "Re-applying config B (must be a no-op) ..."
ids_before=$(docker ps -q --filter "label=k3d.cluster=$clustername" | sort)
$EXE cluster reconfigure "$clustername" -c "$ASSET_DIR/config-b.yaml" \
  || failed "idempotent re-apply failed (note: no --force needed when nothing changes)"
ids_after=$(docker ps -q --filter "label=k3d.cluster=$clustername" | sort)
[ "$ids_before" = "$ids_after" ] || failed "idempotent re-apply replaced containers"
passed "second apply of config B was a no-op"

# --- 5. revocation: applying config A again removes volume/env/arg ---
info "Reverting to config A (revocation) ..."
$EXE cluster reconfigure "$clustername" -c "$ASSET_DIR/config-a.yaml" --force --ready-timeout 300s \
  || failed "revert to config A failed"

for n in "k3d-$clustername-agent-0" "k3d-$clustername-agent-1"; do
  if docker inspect -f '{{range .Mounts}}{{.Destination}} {{end}}' "$n" | grep -q "/mnt/reconftest"; then
    failed "volume mount still present on $n after revocation"
  fi
  if docker inspect -f '{{range .Config.Env}}{{.}} {{end}}' "$n" | grep -q "RECONF_TEST=fromconfig"; then
    failed "env var still present on $n after revocation"
  fi
done
if docker inspect -f '{{range .Config.Cmd}}{{.}} {{end}}' "$server" | grep -q -- "--kube-apiserver-arg=request-timeout=2m30s"; then
  failed "kube-apiserver-arg still present on server after revocation"
fi
check_clusters "$clustername" || failed "cluster unhealthy after revocation"
passed "revocation removed volume, env and arg again"

$EXE cluster delete "$clustername" || failed "could not delete cluster $clustername"

# --- 6. single-server realistic payload: audit policy file + matching arg ---
info "Creating single-server cluster $soloclustername ..."

cat > "$ASSET_DIR/audit-policy.yaml" <<EOF
apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  - level: Metadata
EOF

cat > "$ASSET_DIR/solo-a.yaml" <<EOF
apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: $soloclustername
servers: 1
agents: 0
EOF

cat > "$ASSET_DIR/solo-b.yaml" <<EOF
apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: $soloclustername
servers: 1
agents: 0
volumes:
  - volume: $ASSET_DIR/audit-policy.yaml:/etc/rancher/k3s/audit-policy.yaml
    nodeFilters:
      - server:*
options:
  k3s:
    extraArgs:
      - arg: --kube-apiserver-arg=audit-policy-file=/etc/rancher/k3s/audit-policy.yaml
        nodeFilters:
          - server:*
      - arg: --kube-apiserver-arg=audit-log-path=/var/log/k3s-audit.log
        nodeFilters:
          - server:*
EOF

$EXE cluster create "$soloclustername" --config "$ASSET_DIR/solo-a.yaml" --timeout 360s \
  || failed "could not create cluster $soloclustername"
check_clusters "$soloclustername" || failed "cluster $soloclustername not ready"

info "Applying audit-policy payload with --force ..."
$EXE cluster reconfigure "$soloclustername" -c "$ASSET_DIR/solo-b.yaml" --force --ready-timeout 300s \
  || failed "solo reconfigure failed"

solosrv="k3d-$soloclustername-server-0"
docker inspect -f '{{range .Mounts}}{{.Destination}} {{end}}' "$solosrv" | grep -q "/etc/rancher/k3s/audit-policy.yaml" \
  || failed "audit-policy mount missing on $solosrv"
docker inspect -f '{{range .Config.Cmd}}{{.}} {{end}}' "$solosrv" | grep -q -- "audit-policy-file=/etc/rancher/k3s/audit-policy.yaml" \
  || failed "audit-policy arg missing on $solosrv"
check_clusters "$soloclustername" || failed "cluster $soloclustername unhealthy after reconfigure"

# audit log actually being written proves the apiserver accepted the config
sleep 5
docker exec "$solosrv" sh -c "test -s /var/log/k3s-audit.log" \
  || failed "audit log not written — apiserver did not pick up the audit policy"
passed "audit-policy payload applied and effective"

info "Reverting solo cluster (revocation) ..."
$EXE cluster reconfigure "$soloclustername" -c "$ASSET_DIR/solo-a.yaml" --force --ready-timeout 300s \
  || failed "solo revert failed"
if docker inspect -f '{{range .Config.Cmd}}{{.}} {{end}}' "$solosrv" | grep -q "audit-policy-file"; then
  failed "audit-policy arg still present after revocation"
fi
check_clusters "$soloclustername" || failed "cluster $soloclustername unhealthy after revocation"
passed "solo revocation ok"

$EXE cluster delete "$soloclustername" || failed "could not delete cluster $soloclustername"

exit 0
