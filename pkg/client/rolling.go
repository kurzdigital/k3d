/*
Copyright © 2020-2023 The k3d Author(s)

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/
package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	l "github.com/k3d-io/k3d/v5/pkg/logger"
	k3drt "github.com/k3d-io/k3d/v5/pkg/runtimes"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

// PerNodeOp is the per-node mutation step inside a RollingApply traversal.
// It runs after drain (agents) and before wait-Ready / uncordon, and owns
// whatever it takes to bring this single node into its new state (process
// restart, same-spec recreate, recreate with changeset).
//
// The op may mutate node fields (NodeReplace renames the old container in
// place) — RollingApply restores node.Name afterwards so wait/uncordon
// target the post-op container.
type PerNodeOp func(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, node *k3d.Node) error

// RollingApplyOpts configures a rolling traversal.
type RollingApplyOpts struct {
	// Verb is the present-progressive logging verb, e.g. "Restarting",
	// "Replacing", "Reconfiguring". Used in info logs only.
	Verb string

	// DrainTimeout caps how long `kubectl drain` may run per agent.
	DrainTimeout time.Duration

	// ReadyTimeout caps how long we wait for a node to become Ready after
	// the per-node op completes.
	ReadyTimeout time.Duration

	// Force allows the traversal to proceed despite preconditions that would
	// otherwise abort it: a single-server cluster without external datastore
	// (kube API blackholes for tens of seconds), or servers whose umbrella
	// entrypoint is not installed (no graceful drain/uncordon — see
	// serverEntrypointActive).
	Force bool

	// Op is the per-node mutation. Required.
	Op PerNodeOp

	// Filter, if non-nil, limits the traversal to nodes it returns true
	// for. Skipped nodes are neither drained nor otherwise touched. If no
	// server-role node passes the filter, the server-specific
	// preconditions (single-server API downtime, umbrella entrypoint) are
	// waived — they only guard server replacements.
	Filter func(node *k3d.Node) bool
}

// RollingApply walks the cluster's nodes one at a time, applying Op to each
// after drain (agents) or relying on the k3d entrypoint's SIGTERM-driven
// drain (servers). After the op it waits for the node to report Ready and
// re-enables scheduling (agents only — servers are uncordoned by the
// entrypoint on start).
//
// Order: non-init servers, init server, then agents — control plane before
// kubelets, so the kube version skew policy (kubelet ≤ apiserver) holds
// during the traversal. Etcd quorum survives because each server is replaced
// one at a time.
//
// Preconditions (all abortable, last two overridable with opts.Force):
//   - all servers must be running;
//   - single-server clusters without external datastore;
//   - servers without an active umbrella entrypoint (no graceful
//     drain/uncordon — see serverEntrypointActive).
//
// Failures abort the traversal — partial-state recovery is the operator's
// responsibility.
func RollingApply(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, opts RollingApplyOpts) error {
	if opts.Op == nil {
		return errors.New("RollingApply: opts.Op must not be nil")
	}
	if opts.Verb == "" {
		opts.Verb = "Processing"
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = 60 * time.Second
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = 120 * time.Second
	}

	cluster, err := ClusterGet(ctx, runtime, cluster)
	if err != nil {
		return fmt.Errorf("failed to refresh cluster '%s': %w", cluster.Name, err)
	}

	include := func(n *k3d.Node) bool { return opts.Filter == nil || opts.Filter(n) }

	initServer, servers, agents := partitionNodesForRolling(cluster.Nodes)

	serverAffected := initServer != nil && include(initServer)
	for _, server := range servers {
		if include(server) {
			serverAffected = true
			break
		}
	}

	serverTotal, serversRunning := cluster.ServerCountRunning()
	if err := checkRollingPreconditions(cluster, serverTotal, serversRunning, serverEntrypointActive(cluster), serverAffected, opts.Force); err != nil {
		return err
	}

	// Non-init servers first.
	for _, server := range servers {
		if !include(server) {
			l.Log().Debugf("RollingApply: skipping unaffected server '%s'", server.Name)
			continue
		}
		if err := rollingProcessNode(ctx, runtime, cluster, server, opts); err != nil {
			return fmt.Errorf("failed on server '%s': %w", server.Name, err)
		}
	}

	// Init server next — the other servers are already cycled, so handing
	// off etcd leadership before the swap is safe. The op owns any special
	// prep (etcd member-remove + data-wipe + K3S_URL injection).
	if initServer != nil && include(initServer) {
		if err := rollingProcessNode(ctx, runtime, cluster, initServer, opts); err != nil {
			return fmt.Errorf("failed on init server '%s': %w", initServer.Name, err)
		}
	}

	// Agents last — the whole control plane is cycled first, so a kubelet
	// never ends up newer than the apiserver it talks to.
	for _, agent := range agents {
		if !include(agent) {
			l.Log().Debugf("RollingApply: skipping unaffected agent '%s'", agent.Name)
			continue
		}
		if err := rollingProcessNode(ctx, runtime, cluster, agent, opts); err != nil {
			return fmt.Errorf("failed on agent '%s': %w", agent.Name, err)
		}
	}

	l.Log().Infof("Finished %s cluster '%s'", strings.ToLower(opts.Verb), cluster.Name)
	return nil
}

// rollingProcessNode runs the per-node sequence: refresh cluster snapshot,
// drain (agents only — servers drain themselves via the k3d entrypoint on
// SIGTERM), Op, wait-for-Ready, uncordon (agents only — servers are
// uncordoned by the entrypoint).
func rollingProcessNode(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, node *k3d.Node, opts RollingApplyOpts) error {
	originalName := node.Name
	l.Log().Infof("%s %s node '%s'", opts.Verb, node.Role, originalName)

	isAgent := node.Role == k3d.AgentRole

	// Caller's snapshot can be stale after the first iteration — some ops
	// mutate node pointers in place (NodeReplace renames the old container).
	if fresh, err := ClusterGet(ctx, runtime, cluster); err == nil {
		cluster = fresh
	} else {
		l.Log().Warnf("RollingApply: failed to refresh cluster '%s' (%v) — using stale snapshot, expect later steps to fail with confusing errors", cluster.Name, err)
	}

	if isAgent {
		execServer, err := pickExecServer(cluster, node)
		if err != nil {
			return fmt.Errorf("no running server available to drain agent '%s': %w", originalName, err)
		}
		drainCtx, cancel := context.WithTimeout(ctx, opts.DrainTimeout+30*time.Second)
		// Polite drain via the Evict API (default) so PDBs are honored;
		// --force still removes orphan pods. DrainTimeout caps blocked
		// evictions, after which the timeout branch logs and continues.
		drainCmd := []string{
			"sh", "-c",
			fmt.Sprintf("kubectl drain %s --force --delete-emptydir-data --ignore-daemonsets --timeout=%s", originalName, opts.DrainTimeout),
		}
		drainErr := runtime.ExecInNode(drainCtx, execServer, drainCmd)
		// kubectl's own --timeout fires before our context deadline when
		// evictions can't finish in time (e.g. a slow-starting helm-install Job
		// or kube-system pods that can't reschedule under load). That is a
		// blocked-eviction timeout, not an apiserver/RBAC failure, so treat it
		// like the context-deadline case: warn and continue best-effort.
		drainTimedOut := drainCtx.Err() == context.DeadlineExceeded || kubectlDrainTimedOut(drainErr)
		cancel()
		if drainErr != nil {
			if drainTimedOut {
				l.Log().Warnf("kubectl drain on agent '%s' timed out after %s (continuing — pods may still be terminating): %v", originalName, opts.DrainTimeout, drainErr)
			} else {
				return fmt.Errorf("kubectl drain on agent '%s' failed with a non-timeout error (likely apiserver/RBAC/kubectl issue, not a workload problem): %w", originalName, drainErr)
			}
		}
	}

	if err := opts.Op(ctx, runtime, cluster, node); err != nil {
		return fmt.Errorf("op '%s' on '%s': %w", opts.Verb, originalName, err)
	}

	// Some ops (NodeReplace) mutate node.Name in place. The post-op container
	// keeps the original name, so restore it for the wait/uncordon steps.
	node.Name = originalName

	if err := waitForNodeReady(ctx, runtime, cluster, node, opts.ReadyTimeout); err != nil {
		return fmt.Errorf("node '%s' did not become Ready: %w", originalName, err)
	}

	if isAgent {
		freshCluster, err := ClusterGet(ctx, runtime, cluster)
		if err != nil {
			return fmt.Errorf("failed to refresh cluster before uncordon of '%s': %w", originalName, err)
		}
		execServer, err := pickExecServer(freshCluster, nil)
		if err != nil {
			return fmt.Errorf("no running server available to uncordon agent '%s': %w", originalName, err)
		}
		uncordonCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = runtime.ExecInNode(uncordonCtx, execServer, []string{"sh", "-c", fmt.Sprintf("kubectl uncordon %s", originalName)})
		cancel()
		if err != nil {
			return fmt.Errorf("failed to uncordon agent '%s': %w", originalName, err)
		}
	}

	return nil
}

// kubectlDrainTimedOut reports whether a failed `kubectl drain` was a
// blocked-eviction timeout (pods not terminating within --timeout) rather than
// a hard failure (apiserver unreachable, RBAC, bad invocation). kubectl
// surfaces an expired --timeout through different messages depending on which
// internal wait expired ("global timeout reached" for the overall drain,
// "context deadline exceeded" for a per-pod termination wait), and leaves
// "There are pending pods" behind — none of which indicate a real drain error.
func kubectlDrainTimedOut(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"global timeout reached",
		"context deadline exceeded",
		"There are pending pods",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// partitionNodesForRolling splits nodes into the traversal groups, dropping
// infra (loadbalancer, registry) which is not part of a rolling traversal.
// The returned order — non-init servers, init server, agents — is the order
// RollingApply processes them in.
func partitionNodesForRolling(nodes []*k3d.Node) (initServer *k3d.Node, servers []*k3d.Node, agents []*k3d.Node) {
	for _, n := range nodes {
		switch n.Role {
		case k3d.ServerRole:
			if n.ServerOpts.IsInit {
				initServer = n
			} else {
				servers = append(servers, n)
			}
		case k3d.AgentRole:
			agents = append(agents, n)
		}
	}
	return initServer, servers, agents
}

// serverEntrypointActive reports whether every server node of the cluster
// actually runs through the k3d umbrella entrypoint (/bin/k3d-entrypoint.sh),
// which drives the SIGTERM drain + start uncordon on server nodes. This is
// derived from the containers' real entrypoint (reconstructed by the runtime
// on inspect), not from this process' K3D_FIX_* environment — the cluster may
// have been created in a different environment than the one the rolling op
// runs in.
func serverEntrypointActive(cluster *k3d.Cluster) bool {
	for _, n := range cluster.Nodes {
		if n.Role != k3d.ServerRole {
			continue
		}
		if !n.K3dEntrypoint {
			return false
		}
	}
	return true
}

// clusterHasExternalDatastore reports whether the cluster's servers run
// against an external datastore. Cluster.ExternalDatastore is only populated
// on the create path — a cluster reconstructed from containers never carries
// it — so inspect the server nodes' env and k3s args instead.
func clusterHasExternalDatastore(cluster *k3d.Cluster) bool {
	for _, n := range cluster.Nodes {
		if n.Role != k3d.ServerRole {
			continue
		}
		for _, env := range n.Env {
			if strings.HasPrefix(env, "K3S_DATASTORE_ENDPOINT=") {
				return true
			}
		}
		for _, arg := range append(append([]string{}, n.Cmd...), n.Args...) {
			if arg == "--datastore-endpoint" || strings.HasPrefix(arg, "--datastore-endpoint=") {
				return true
			}
		}
	}
	return false
}

// checkRollingPreconditions validates a cluster against the abortable
// preconditions of a rolling traversal. entrypointActive reports whether all
// server nodes run through the umbrella entrypoint (see
// serverEntrypointActive); serverAffected reports whether any server-role
// node is part of the traversal — the server-specific checks only apply
// then. The latter two checks are overridable with force.
func checkRollingPreconditions(cluster *k3d.Cluster, serverTotal, serversRunning int, entrypointActive, serverAffected, force bool) error {
	if serversRunning < serverTotal {
		return fmt.Errorf("cluster '%s' has %d/%d servers running — start the cluster first", cluster.Name, serversRunning, serverTotal)
	}

	if !serverAffected {
		return nil
	}

	if !force && cluster.ExternalDatastore == nil && !clusterHasExternalDatastore(cluster) && serversRunning < 2 {
		return fmt.Errorf("cluster '%s' has only %d server(s) and no external datastore — applying a rolling op will make the kube API unavailable for tens of seconds. Pass --force to proceed anyway", cluster.Name, serversRunning)
	}

	if serverTotal > 0 && !entrypointActive {
		if !force {
			return fmt.Errorf("cluster '%s' servers have no active k3d entrypoint (no K3D_FIX_* enabled) — server nodes will be cycled WITHOUT graceful drain/uncordon, risking abrupt workload eviction and a non-schedulable node on failure. Enable a fix (e.g. K3D_FIX_MOUNTS=true) or pass --force to proceed anyway", cluster.Name)
		}
		l.Log().Warnf("cluster '%s' servers have no active k3d entrypoint — proceeding due to --force, but server nodes will be cycled WITHOUT graceful drain/uncordon", cluster.Name)
	}

	return nil
}

// pickExecServer returns a running server-role node from the cluster,
// preferring one that is not the node currently being operated on. Falls
// back to the target itself if no other server is available (single-server
// clusters with --force) — in that case the caller may still hit a brief
// API window where kubectl can't reach the apiserver.
func pickExecServer(cluster *k3d.Cluster, skip *k3d.Node) (*k3d.Node, error) {
	var fallback *k3d.Node
	for _, n := range cluster.Nodes {
		if n.Role != k3d.ServerRole {
			continue
		}
		if !n.State.Running {
			continue
		}
		if skip != nil && n.Name == skip.Name {
			fallback = n
			continue
		}
		return n, nil
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, fmt.Errorf("no running server-role node found in cluster '%s'", cluster.Name)
}

// waitForNodeReady polls `kubectl wait --for=condition=Ready` from a running
// server until the target reports Ready=True or the timeout expires. Each
// individual exec is bounded by its own context, so a stuck server cannot
// block the loop indefinitely.
func waitForNodeReady(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, target *k3d.Node, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pollInterval := 2 * time.Second
	const perCallTimeout = 10 * time.Second // kubectl wait --timeout=5s + buffer

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		fresh, err := ClusterGet(ctx, runtime, cluster)
		if err != nil {
			l.Log().Debugf("waitForNodeReady: refresh failed (%v) — retrying", err)
		} else if execServer, perr := pickExecServer(fresh, target); perr == nil {
			check := []string{
				"sh", "-c",
				fmt.Sprintf("kubectl wait --for=condition=Ready node/%s --timeout=5s", target.Name),
			}
			callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
			err := runtime.ExecInNode(callCtx, execServer, check)
			cancel()
			if err == nil {
				l.Log().Debugf("Node '%s' reports Ready", target.Name)
				return nil
			}
			l.Log().Tracef("waitForNodeReady: kubectl wait on '%s' via '%s' returned: %v", target.Name, execServer.Name, err)
		} else {
			l.Log().Debugf("waitForNodeReady: no exec server available yet (%v)", perr)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("node '%s' not Ready after %s", target.Name, timeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
