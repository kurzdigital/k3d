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

export CURRENT_STAGE="Test | cluster rollout"

clustername_ha="rollout-ha"
clustername_solo="rollout-solo"

cleanup() {
  if [[ -n "$E2E_KEEP" ]]; then
    info "E2E_KEEP set — skipping cluster cleanup so state can be inspected"
    return
  fi
  $EXE cluster delete "$clustername_ha"   >/dev/null 2>&1 || true
  $EXE cluster delete "$clustername_solo" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- 1. HA cluster: rollout should succeed without --force ---

info "Creating HA cluster '$clustername_ha' (3 servers, 2 agents) ..."
# DIND has tight disk; the default kubelet eviction-hard at 5% trips
# trivially under the load of a 5-node cluster, leaving pause pods
# Pending with disk-pressure taints. Push the threshold down so the
# test isn't at the mercy of DIND filesystem usage.
$EXE cluster create "$clustername_ha" \
  --servers 3 --agents 2 \
  --k3s-arg '--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%@server:*;agent:*' \
  --wait --timeout 360s \
  || failed "could not create HA cluster '$clustername_ha'"

check_clusters "$clustername_ha"            || failed "HA cluster '$clustername_ha' not reachable"
check_multi_node "$clustername_ha" 5         || failed "expected 5 nodes in '$clustername_ha'"

info "Deploying a 3-replica workload to exercise drain/reschedule paths ..."
KUBECTL_HA_CTX="k3d-${clustername_ha}"
kubectl --context "$KUBECTL_HA_CTX" create deployment rolling-pause --image=registry.k8s.io/pause:3.9 --replicas=3 \
  || failed "could not create rolling-pause deployment"
kubectl --context "$KUBECTL_HA_CTX" rollout status deployment/rolling-pause --timeout=240s \
  || {
    info "Rollout stuck — dumping pod state for diagnosis ..."
    kubectl --context "$KUBECTL_HA_CTX" get pods -o wide || true
    failed "rolling-pause never became ready before rollout"
  }

info "Capturing container IDs before rollout ..."
before_ids=$(docker ps --filter "label=k3d.cluster=$clustername_ha" --format '{{.ID}}' | sort)

info "Rolling-restart of '$clustername_ha' ..."
$EXE cluster rollout "$clustername_ha" --ready-timeout 180s \
  || failed "rollout of '$clustername_ha' failed"

info "Verifying rolling-pause is still healthy after rollout ..."
kubectl --context "$KUBECTL_HA_CTX" rollout status deployment/rolling-pause --timeout=120s \
  || {
    info "Workload didn't come back — dumping pod state for diagnosis ..."
    kubectl --context "$KUBECTL_HA_CTX" get pods -o wide --all-namespaces || true
    kubectl --context "$KUBECTL_HA_CTX" describe deployment rolling-pause || true
    kubectl --context "$KUBECTL_HA_CTX" describe pods -l app=rolling-pause || true
    kubectl --context "$KUBECTL_HA_CTX" get nodes || true
    failed "rolling-pause not Ready after rollout"
  }
ready_replicas=$(kubectl --context "$KUBECTL_HA_CTX" get deployment rolling-pause -o jsonpath='{.status.readyReplicas}')
if [[ "$ready_replicas" -ne 3 ]]; then
  failed "rolling-pause has $ready_replicas/3 ready replicas after rollout"
fi

info "Verifying container identity is preserved (rollout must not recreate containers) ..."
after_ids=$(docker ps --filter "label=k3d.cluster=$clustername_ha" --format '{{.ID}}' | sort)
if [[ "$before_ids" != "$after_ids" ]]; then
  failed "container IDs changed across rollout — that should not happen, restart must preserve containers (before:\n$before_ids\nafter:\n$after_ids)"
fi

info "Verifying cluster is still healthy after rollout ..."
# kubelet Ready (which our rollout waits for) precedes the apiserver
# being fully up by a few seconds for the last-restarted server. Give it a
# short window with a few retries before declaring the cluster broken.
healthy=0
for i in $(seq 1 10); do
  if check_clusters "$clustername_ha" >/dev/null 2>&1; then
    healthy=1
    break
  fi
  sleep 3
done
if [[ "$healthy" -ne 1 ]]; then
  check_clusters "$clustername_ha" || failed "cluster '$clustername_ha' unreachable after rollout (waited ~30s)"
fi
check_multi_node "$clustername_ha" 5 || failed "lost a node during rollout"

passed "HA rollout succeeded (containers preserved, workload survived)"

# --- 2. Single-server cluster: should refuse without --force, succeed with it ---

info "Creating single-server cluster '$clustername_solo' ..."
$EXE cluster create "$clustername_solo" \
  --servers 1 --agents 1 \
  --wait --timeout 180s \
  || failed "could not create solo cluster '$clustername_solo'"

info "Rolling-restart WITHOUT --force should refuse on single-server cluster ..."
if $EXE cluster rollout "$clustername_solo" --ready-timeout 180s; then
  failed "rollout on single-server cluster succeeded without --force (expected refusal)"
fi
info "Refusal verified."

info "Rolling-restart WITH --force should succeed ..."
$EXE cluster rollout "$clustername_solo" --force --ready-timeout 180s \
  || failed "forced rollout on single-server cluster failed"

passed "single-server forced rollout succeeded"

exit 0
