#!/bin/bash

CURR_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
[ -d "$CURR_DIR" ] || { echo "FATAL: no current dir (maybe running in zsh?)";  exit 1; }

# shellcheck source=./common.sh
source "$CURR_DIR/common.sh"

### Step Setup ###
# Redirect all stdout/stderr output to logfile
LOG_FILE="$TEST_OUTPUT_DIR/$( basename "${BASH_SOURCE[0]}" ).log"
exec >${LOG_FILE} 2>&1
export LOG_FILE

# use a kubeconfig file specific to this test
KUBECONFIG="$KUBECONFIG_ROOT/$( basename "${BASH_SOURCE[0]}" ).yaml"
export KUBECONFIG
### Step Setup ###

export CURRENT_STAGE="Test | IPAM static IP conflict"

highlight "[START] IPAM static IP conflict $EXTRA_TITLE"

# A stopped server node releases its static IP. If docker hands out dynamic IPs from the same pool
# (e.g. when the daemon restarts all containers in parallel), another container can take it and the
# server fails to start with "Address already in use". Simulate that by exhausting the dynamic pool.

clustername="ipamconflicttest"
subnet="172.81.0.0/27"
expectedIPRange="172.81.0.16/28"
dummyLabel="k3d.test.ipamconflict"

cleanup() {
  docker ps -aq --filter "label=$dummyLabel" | xargs -r docker rm -f
}

info "Creating cluster $clustername..."
$EXE cluster create $clustername --timeout 360s --subnet $subnet --agents 1 || failed "could not create cluster $clustername"

if [[ $(docker network inspect k3d-$clustername | jq -r '.[0].IPAM.Config[0].IPRange') != "$expectedIPRange" ]]; then
  failed "IP range does not match expected value: $(docker network inspect k3d-$clustername | jq -r '.[0].IPAM.Config[0].IPRange') != $expectedIPRange"
fi

serverIP=$(docker inspect k3d-$clustername-server-0 | jq -r '.[0].Config.Labels["k3d.node.staticIP"]')
info "Server k3d-$clustername-server-0 has static IP $serverIP"

info "Stopping server node and exhausting the dynamic IP pool..."
docker stop k3d-$clustername-server-0 || failed "could not stop k3d-$clustername-server-0"
for _ in $(seq 1 40); do
  docker run -d --label "$dummyLabel" --network k3d-$clustername alpine:3 sleep 600 >/dev/null 2>&1 || break
done
info "Dummy containers running: $(docker ps -q --filter "label=$dummyLabel" | wc -l)"

for dummy in $(docker ps -q --filter "label=$dummyLabel"); do
  if [[ $(docker inspect "$dummy" | jq -r ".[0].NetworkSettings.Networks[\"k3d-$clustername\"].IPAddress") == "$serverIP" ]]; then
    cleanup
    failed "dynamically started container $dummy took the static IP $serverIP of k3d-$clustername-server-0"
  fi
done

info "Starting server node again..."
docker start k3d-$clustername-server-0 || { cleanup; failed "could not start k3d-$clustername-server-0 with its static IP $serverIP"; }

cleanup

info "Deleting cluster $clustername..."
$EXE cluster delete $clustername || failed "could not delete the cluster $clustername"

exit 0
