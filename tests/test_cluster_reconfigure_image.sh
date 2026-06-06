#!/bin/bash

CURR_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
[ -d "$CURR_DIR" ] || { echo "FATAL: no current dir (maybe running in zsh?)";  exit 1; }

# shellcheck source=./common.sh
source "$CURR_DIR/common.sh"

### Step Setup ###
LOG_FILE="$TEST_OUTPUT_DIR/$( basename "${BASH_SOURCE[0]}" ).log"
exec >${LOG_FILE} 2>&1
export LOG_FILE

KUBECONFIG="$KUBECONFIG_ROOT/$( basename "${BASH_SOURCE[0]}" ).yaml"
export KUBECONFIG
### Step Setup ###

export CURRENT_STAGE="Test | cluster reconfigure --image"

# Cross-minor upgrade. With the etcd member rotation now applied to every
# server (incl. init), no server is left on the old minor — kubelets on
# the upgraded nodes never end up newer than any apiserver they talk to,
# so the k8s version skew policy is honored throughout.
: "${RECONFIGURE_FROM_IMAGE:=rancher/k3s:v1.30.5-k3s1}"
: "${RECONFIGURE_TO_IMAGE:=rancher/k3s:v1.31.0-k3s1}"

clustername_ha="reconfig-ha"
clustername_solo="reconfig-solo"

cleanup() {
  if [[ -n "$E2E_KEEP" ]]; then
    info "E2E_KEEP set — skipping cluster cleanup so state can be inspected"
    return
  fi
  $EXE cluster delete "$clustername_ha" >/dev/null 2>&1 || true
  $EXE cluster delete "$clustername_solo" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- 1. HA cluster: rolling image upgrade should succeed without --force ---

info "Creating HA cluster '$clustername_ha' (3 servers, 2 agents) on $RECONFIGURE_FROM_IMAGE ..."
# DIND has tight disk; the default kubelet eviction-hard at 5% trips
# trivially under the load of a 5-node cluster's image and container
# churn during reconfigure. Push the threshold down so the test isn't
# at the mercy of DIND filesystem usage.
$EXE cluster create "$clustername_ha" \
  --servers 3 --agents 2 \
  --image "$RECONFIGURE_FROM_IMAGE" \
  --k3s-arg '--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%@server:*;agent:*' \
  --wait --timeout 360s \
  || failed "could not create HA cluster '$clustername_ha'"

check_clusters "$clustername_ha" || failed "HA cluster '$clustername_ha' not reachable"
check_multi_node "$clustername_ha" 5 || failed "expected 5 nodes in '$clustername_ha'"

info "Verifying all nodes are running on $RECONFIGURE_FROM_IMAGE ..."
expected_count=5
actual_count=$(docker ps --filter "label=k3d.cluster=$clustername_ha" --format '{{.Image}}' | grep -c "^${RECONFIGURE_FROM_IMAGE}$" || true)
if [[ "$actual_count" -ne "$expected_count" ]]; then
  failed "expected $expected_count containers on $RECONFIGURE_FROM_IMAGE, found $actual_count"
fi

info "Deploying a 3-replica drain-tolerant workload (anti-affinity + PDB) ..."
KUBECTL_HA_CTX="k3d-${clustername_ha}"
# - pause image so DIND ephemeral-storage doesn't trip the eviction manager.
# - podAntiAffinity (required, hostname topology) spreads pods across
#   distinct nodes; with 3 replicas + 5 nodes the scheduler always has slack.
# - PodDisruptionBudget minAvailable=1 makes Evict-based drain wait until
#   a replacement pod is Ready elsewhere. Combined with our cluster
#   reconfigure (which now uses the Evict API) this turns drain → wait →
#   replace into a coherent sequence with no oscillation.
kubectl --context "$KUBECTL_HA_CTX" apply -f - <<'YAML' || failed "could not create rolling-pause workload"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: rolling-pause
spec:
  replicas: 3
  selector:
    matchLabels: { app: rolling-pause }
  template:
    metadata:
      labels: { app: rolling-pause }
    spec:
      containers:
      - { name: pause, image: registry.k8s.io/pause:3.9 }
      affinity:
        podAntiAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
          - labelSelector:
              matchExpressions:
              - { key: app, operator: In, values: [rolling-pause] }
            topologyKey: kubernetes.io/hostname
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: rolling-pause-pdb
spec:
  minAvailable: 1
  selector:
    matchLabels: { app: rolling-pause }
YAML
kubectl --context "$KUBECTL_HA_CTX" rollout status deployment/rolling-pause --timeout=240s \
  || {
    info "Rollout stuck — dumping pod state for diagnosis ..."
    kubectl --context "$KUBECTL_HA_CTX" get pods -o wide || true
    kubectl --context "$KUBECTL_HA_CTX" describe deployment rolling-pause || true
    failed "rolling-pause never became ready before reconfigure"
  }

info "Reconfiguring '$clustername_ha' to $RECONFIGURE_TO_IMAGE ..."
$EXE cluster reconfigure "$clustername_ha" --image "$RECONFIGURE_TO_IMAGE" --ready-timeout 180s \
  || failed "rolling reconfigure of '$clustername_ha' failed"

info "Verifying rolling-pause is still healthy after reconfigure ..."
# `kubectl rollout status` is too eager here: every eviction cycle during
# reconfigure makes it report "0 of 3 updated replicas" even though the
# deployment template hasn't actually changed. Use `wait --for=condition=
# Available` instead — it just checks ReadyReplicas == DesiredReplicas
# without the rolling-update semantics. Generous timeout because DIND is
# disk-tight and recovery has a long tail.
kubectl --context "$KUBECTL_HA_CTX" wait deployment/rolling-pause --for=condition=Available --timeout=600s \
  || {
    info "Workload didn't recover — dumping pod state for diagnosis ..."
    kubectl --context "$KUBECTL_HA_CTX" get pods -o wide --all-namespaces || true
    kubectl --context "$KUBECTL_HA_CTX" describe deployment rolling-pause || true
    kubectl --context "$KUBECTL_HA_CTX" describe pods -l app=rolling-pause || true
    kubectl --context "$KUBECTL_HA_CTX" get nodes || true
    failed "rolling-pause not Ready after reconfigure (drain/reschedule broke the workload)"
  }
ready_replicas=$(kubectl --context "$KUBECTL_HA_CTX" get deployment rolling-pause -o jsonpath='{.status.readyReplicas}')
if [[ "$ready_replicas" -ne 3 ]]; then
  failed "rolling-pause has $ready_replicas/3 ready replicas after reconfigure"
fi

info "Verifying all nodes now run on $RECONFIGURE_TO_IMAGE ..."
# Etcd member rotation lets us replace the init server too — all 5
# containers (3 servers + 2 agents) end up on the new image.
expected_upgraded=5
upgraded_count=$(docker ps --filter "label=k3d.cluster=$clustername_ha" --format '{{.Image}}' | grep -c "^${RECONFIGURE_TO_IMAGE}$" || true)
if [[ "$upgraded_count" -ne "$expected_upgraded" ]]; then
  failed "expected $expected_upgraded containers on $RECONFIGURE_TO_IMAGE, found $upgraded_count"
fi

info "Verifying cluster is still healthy after rolling upgrade ..."
check_clusters "$clustername_ha" || failed "cluster '$clustername_ha' unreachable after reconfigure"
check_multi_node "$clustername_ha" 5 || failed "lost a node during reconfigure"

passed "HA rolling upgrade succeeded (all 5 nodes upgraded incl. init server via etcd member rotation)"

# --- 2. Single-server cluster: should refuse without --force, succeed with it ---

info "Creating single-server cluster '$clustername_solo' on $RECONFIGURE_FROM_IMAGE ..."
$EXE cluster create "$clustername_solo" \
  --servers 1 --agents 1 \
  --image "$RECONFIGURE_FROM_IMAGE" \
  --wait --timeout 180s \
  || failed "could not create solo cluster '$clustername_solo'"

info "Reconfigure WITHOUT --force should refuse on single-server cluster ..."
if $EXE cluster reconfigure "$clustername_solo" --image "$RECONFIGURE_TO_IMAGE" --ready-timeout 180s; then
  failed "reconfigure on single-server cluster succeeded without --force (expected refusal)"
fi
info "Refusal verified."

info "Reconfigure WITH --force should succeed ..."
$EXE cluster reconfigure "$clustername_solo" --image "$RECONFIGURE_TO_IMAGE" --force --ready-timeout 180s \
  || failed "forced reconfigure on single-server cluster failed"

upgraded_count_solo=$(docker ps --filter "label=k3d.cluster=$clustername_solo" --format '{{.Image}}' | grep -c "^${RECONFIGURE_TO_IMAGE}$" || true)
if [[ "$upgraded_count_solo" -ne 2 ]]; then
  failed "expected 2 containers on $RECONFIGURE_TO_IMAGE in solo cluster, found $upgraded_count_solo"
fi

passed "single-server forced upgrade succeeded"

# Cleanup runs via trap.
exit 0
