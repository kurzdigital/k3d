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
	"fmt"
	"time"

	l "github.com/k3d-io/k3d/v5/pkg/logger"
	k3drt "github.com/k3d-io/k3d/v5/pkg/runtimes"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

// ClusterRolloutOpts configures a rolling-restart run.
type ClusterRolloutOpts struct {
	// Force allows restart on a single-server cluster despite the resulting
	// API downtime.
	Force bool

	// DrainTimeout caps how long `kubectl drain` may run per agent.
	DrainTimeout time.Duration

	// ReadyTimeout caps how long we wait for a restarted node to become
	// Ready before failing the run.
	ReadyTimeout time.Duration
}

// ClusterRollout cycles a cluster's nodes one at a time: drain (agents)
// → stop → start → wait Ready → uncordon (agents). Servers drain and
// uncordon themselves via the k3d entrypoint's SIGTERM/start hooks.
//
// Containers are preserved — only the k3s process and kubelet inside them
// are replaced, so state on /var/lib/rancher/k3s is retained. This is a
// rolling-restart (à la `kubectl rollout restart`), not a substitute for
// `cluster restart`: it does not re-run the cluster-level post-start phase
// (HostAliases injection, CoreDNS configmap rewrite) needed after host-side
// changes (WLAN switch, Docker daemon restart, network recreate).
func ClusterRollout(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, opts ClusterRolloutOpts) error {
	// Gather environment info up-front so per-node NodeStart can hand it to
	// the enableFixes hook (DNS magic etc.), as cluster restart does.
	freshCluster, err := ClusterGet(ctx, runtime, cluster)
	if err != nil {
		return fmt.Errorf("failed to refresh cluster '%s': %w", cluster.Name, err)
	}
	envInfo, err := GatherEnvironmentInfo(ctx, runtime, freshCluster)
	if err != nil {
		return fmt.Errorf("failed to gather environment info for cluster '%s': %w", cluster.Name, err)
	}

	op := makeRestartOp(envInfo)
	return RollingApply(ctx, runtime, cluster, RollingApplyOpts{
		Verb:         "Restarting",
		Force:        opts.Force,
		DrainTimeout: opts.DrainTimeout,
		ReadyTimeout: opts.ReadyTimeout,
		Op:           op,
	})
}

// makeRestartOp returns a PerNodeOp that stops+starts the existing
// container. The k3d entrypoint's SIGTERM handler drains servers; agents are
// drained by RollingApply first. envInfo is captured once and reused (the
// HostGateway address doesn't change mid-run).
func makeRestartOp(envInfo *k3d.EnvironmentInfo) PerNodeOp {
	return func(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, node *k3d.Node) error {
		l.Log().Debugf("Stopping node '%s'", node.Name)
		if err := runtime.StopNode(ctx, node); err != nil {
			return fmt.Errorf("runtime failed to stop node '%s': %w", node.Name, err)
		}
		// runtime.StopNode does not update the in-memory State; NodeStart
		// would otherwise see the stale `Running=true` and return without
		// actually starting the container.
		node.State.Running = false

		l.Log().Debugf("Starting node '%s'", node.Name)
		startOpts := &k3d.NodeStartOpts{
			Wait:            true,
			NodeHooks:       node.HookActions,
			EnvironmentInfo: envInfo,
		}
		if err := NodeStart(ctx, runtime, node, startOpts); err != nil {
			return fmt.Errorf("failed to start node '%s' back up: %w", node.Name, err)
		}
		return nil
	}
}
