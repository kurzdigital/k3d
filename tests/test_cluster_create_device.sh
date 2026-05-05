#!/bin/bash

CURR_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
[ -d "$CURR_DIR" ] || { echo "FATAL: no current dir (maybe running in zsh?)"; exit 1; }

# shellcheck source=./common.sh
source "$CURR_DIR/common.sh"

### Step Setup ###
LOG_FILE="$TEST_OUTPUT_DIR/$( basename "${BASH_SOURCE[0]}" ).log"
exec >${LOG_FILE} 2>&1
export LOG_FILE

KUBECONFIG="$KUBECONFIG_ROOT/$( basename "${BASH_SOURCE[0]}" ).yaml"
export KUBECONFIG
### Step Setup ###

export CURRENT_STAGE="Test | DeviceFlag"

clustername="devicetest"

# We use /dev/null and /dev/zero because they exist on every Linux host and are harmless.
# Two specs cover both supported path-style forms:
#   --device /dev/null:/dev/k3d-null:rw  -> host:container[:perms] remap
#   --device /dev/zero                   -> host == container, default perms
# CDI-style ('vendor/class=name') is intentionally not exercised here: it requires a
# CDI-aware container runtime configured on the DIND host, which we don't provide.
info "Creating cluster $clustername with --device (path remap + path-only)..."
$EXE cluster create $clustername \
  --timeout 360s \
  --servers 1 \
  --agents 1 \
  --device "/dev/null:/dev/k3d-null:rw" \
  --device "/dev/zero" \
  || failed "could not create cluster $clustername"

info "Checking we have access to the cluster..."
check_clusters "$clustername" || failed "error checking cluster"

for node in "k3d-$clustername-server-0" "k3d-$clustername-agent-0"; do
  info "Checking devices on $node..."

  exec_in_node "$node" "test -c /dev/k3d-null" \
    || failed "$node: /dev/k3d-null missing or not a char device (host:container remap broken)"
  exec_in_node "$node" "test -c /dev/zero" \
    || failed "$node: /dev/zero missing"

  devices_json=$(docker inspect "$node" --format '{{ json .HostConfig.Devices }}')
  echo "$devices_json" | grep -q '"PathInContainer":"/dev/k3d-null"' \
    || failed "$node: HostConfig.Devices missing /dev/k3d-null mapping ($devices_json)"
  echo "$devices_json" | grep -q '"PathInContainer":"/dev/zero"' \
    || failed "$node: HostConfig.Devices missing /dev/zero mapping ($devices_json)"
done

info "Checking the load balancer is NOT given the devices..."
lb_devices=$(docker inspect "k3d-$clustername-serverlb" --format '{{ json .HostConfig.Devices }}' 2>/dev/null || echo "null")
if [[ "$lb_devices" != "[]" && "$lb_devices" != "null" ]]; then
  failed "load balancer received unexpected devices: $lb_devices"
fi

info "Rejecting an invalid --device spec..."
# Anything that's neither a /dev path nor <vendor>/<class>=<name> must fail at create time.
if $EXE cluster create "${clustername}-bad" --timeout 30s --device "garbage-spec" 2>/dev/null; then
  $EXE cluster delete "${clustername}-bad" >/dev/null 2>&1 || true
  failed "cluster creation accepted an invalid --device spec ('garbage-spec')"
fi

info "Deleting cluster..."
$EXE cluster delete $clustername || failed "could not delete cluster $clustername"

exit 0
