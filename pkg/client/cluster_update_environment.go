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

	l "github.com/k3d-io/k3d/v5/pkg/logger"
	k3drt "github.com/k3d-io/k3d/v5/pkg/runtimes"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

// ClusterUpdateEnvironment refreshes the cluster's environment-derived
// state — the host-gateway entry in /etc/hosts of every server/agent
// (`host.k3d.internal`) and the CoreDNS configmap's NodeHosts list —
// against the host's CURRENT state. Does NOT restart any container.
//
// Use this after a host-side network change that's left the cluster's
// view of the host stale: a WLAN/network switch (gateway IP changed),
// a Docker daemon restart (bridge re-created with a new subnet), or a
// `docker network` recreate. `cluster restart` (bulk) does the same
// reconciliation as a side effect — `cluster update-environment` is
// the surgical version, no pod cycling.
//
// Limitations:
//   - User-provided HostAliases from cluster create time are NOT
//     re-injected here (we don't track them in container labels). Only
//     the auto-managed host.k3d.internal entry plus the network member
//     records are refreshed. Custom aliases need a `cluster restart` to
//     come back from clusterStartOpts.
//   - In `host` network mode, this function is a no-op (the host's
//     resolver IS the cluster's resolver).
func ClusterUpdateEnvironment(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster) error {
	// Refresh cluster so we operate on the live node list and current IPs.
	cluster, err := ClusterGet(ctx, runtime, cluster)
	if err != nil {
		return fmt.Errorf("failed to refresh cluster '%s': %w", cluster.Name, err)
	}

	envInfo, err := GatherEnvironmentInfo(ctx, runtime, cluster)
	if err != nil {
		return fmt.Errorf("failed to gather environment info for cluster '%s': %w", cluster.Name, err)
	}

	// Only running server/agent nodes are candidates for /etc/hosts
	// injection. Stopped containers will pick up the right state when
	// they next start (ClusterStart re-runs this same helper).
	var (
		servers []*k3d.Node
		agents  []*k3d.Node
	)
	for _, n := range cluster.Nodes {
		if !n.State.Running {
			continue
		}
		switch n.Role {
		case k3d.ServerRole:
			servers = append(servers, n)
		case k3d.AgentRole:
			agents = append(agents, n)
		}
	}

	if len(servers) == 0 && len(agents) == 0 {
		return fmt.Errorf("cluster '%s' has no running server/agent nodes — start it first", cluster.Name)
	}

	l.Log().Infof("Refreshing environment-derived state on cluster '%s' (host-gateway %s)", cluster.Name, envInfo.HostGateway)
	return applyClusterEnvironment(ctx, runtime, cluster, envInfo, nil, servers, agents)
}
