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

export CURRENT_STAGE="Test | cluster update-environment"

clustername="updateenv"
# Declared early so the cleanup trap is robust even if cluster create fails
# before the stranger container (and thus this variable) is set up.
strangerName=""

cleanup() {
  if [[ -n "$E2E_KEEP" ]]; then
    info "E2E_KEEP set — skipping cluster cleanup"
    return
  fi
  $EXE cluster delete "$clustername" >/dev/null 2>&1 || true
  docker rm -f "$strangerName" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- Create cluster, capture initial CoreDNS NodeHosts ---

info "Creating cluster '$clustername' (1 server, 1 agent) ..."
$EXE cluster create "$clustername" \
  --servers 1 --agents 1 \
  --wait --timeout 180s \
  || failed "could not create cluster '$clustername'"

CTX="k3d-${clustername}"

info "Capturing initial CoreDNS configmap NodeHosts ..."
before=$(kubectl --context "$CTX" -n kube-system get configmap coredns -o jsonpath='{.data.NodeHosts}' 2>/dev/null)
if [[ -z "$before" ]]; then
  failed "CoreDNS configmap not found or NodeHosts empty after cluster create"
fi
info "Initial NodeHosts:"
echo "$before"

# --- Add a stranger container to the cluster network, run update-environment ---

# Find the cluster network name.
network=$(docker network ls --filter "label=app=k3d" --filter "name=k3d-${clustername}" --format '{{.Name}}' | head -n1)
if [[ -z "$network" ]]; then
  failed "could not locate cluster network for '$clustername'"
fi

strangerName="updateenv-stranger-$(date +%s)"
info "Adding stranger container '$strangerName' to network '$network' ..."
docker run -d --rm --name "$strangerName" --network "$network" registry.k8s.io/pause:3.9 >/dev/null \
  || failed "could not start stranger container"

info "Running cluster update-environment ..."
$EXE cluster update-environment "$clustername" \
  || failed "cluster update-environment failed"

# Retry: k3s needs a moment to apply the rewritten configmap, and under
# DIND load a single fixed sleep is flaky.
info "Capturing post-update NodeHosts (retrying up to 60s) ..."
after=""
for i in $(seq 1 12); do
  after=$(kubectl --context "$CTX" -n kube-system get configmap coredns -o jsonpath='{.data.NodeHosts}' 2>/dev/null)
  if [[ -n "$after" ]] && echo "$after" | grep -q "$strangerName"; then
    break
  fi
  sleep 5
done
echo "$after"

if [[ -z "$after" ]]; then
  failed "CoreDNS NodeHosts is empty after update-environment"
fi

if ! echo "$after" | grep -q "$strangerName"; then
  info "BEFORE:"
  echo "$before"
  info "AFTER:"
  echo "$after"
  failed "stranger container '$strangerName' did not appear in CoreDNS NodeHosts after update-environment (after 60s)"
fi

passed "update-environment refreshed CoreDNS to include new network member"

# --- Sanity: host.k3d.internal still present in /etc/hosts on a node ---

info "Verifying host.k3d.internal entry on cluster nodes ..."
for n in "k3d-${clustername}-server-0" "k3d-${clustername}-agent-0"; do
  if ! docker exec "$n" grep -q "host.k3d.internal" /etc/hosts; then
    failed "host.k3d.internal missing from /etc/hosts on $n"
  fi
done
passed "host.k3d.internal entry present on all nodes"

exit 0
