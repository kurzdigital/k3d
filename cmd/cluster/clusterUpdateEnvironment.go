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
package cluster

import (
	"github.com/spf13/cobra"

	cliutil "github.com/k3d-io/k3d/v5/cmd/util"
	"github.com/k3d-io/k3d/v5/pkg/client"
	l "github.com/k3d-io/k3d/v5/pkg/logger"
	"github.com/k3d-io/k3d/v5/pkg/runtimes"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

// NewCmdClusterUpdateEnvironment returns a new cobra command for
// `k3d cluster update-environment`.
func NewCmdClusterUpdateEnvironment() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update-environment CLUSTER",
		Short: "[EXPERIMENTAL] Refresh cluster's view of the host (gateway, network members) without restart",
		Long: `[EXPERIMENTAL] Refresh environment-derived cluster state.

Re-injects the auto-managed ` + "`host.k3d.internal`" + ` entry into /etc/hosts on
every running server/agent and rewrites the CoreDNS configmap's
NodeHosts list against the cluster's current network members. Does NOT
restart any container.

Use this after a host-side network change that's left the cluster's
view of the host stale:

  - WLAN switch / network change → host gateway IP changed.
  - Docker daemon restart → bridge re-created with a different subnet.
  - ` + "`docker network`" + ` recreate.

Bulk ` + "`cluster restart`" + ` does the same reconciliation as a side effect of
restarting; this verb is the surgical version that skips the pod cycling.

Caveats:
  - User-provided HostAliases from ` + "`cluster create`" + ` aren't re-injected
    (k3d doesn't persist them in container labels). Use ` + "`cluster restart`" + `
    to get those back.
  - No-op on clusters running in 'host' network mode.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: cliutil.ValidArgsAvailableClusters,
		Run: func(cmd *cobra.Command, args []string) {
			cluster, err := client.ClusterGet(cmd.Context(), runtimes.SelectedRuntime, &k3d.Cluster{Name: args[0]})
			if err != nil {
				l.Log().Fatalln(err)
			}
			if cluster == nil {
				l.Log().Fatalf("Cluster %s not found", args[0])
			}

			if err := client.ClusterUpdateEnvironment(cmd.Context(), runtimes.SelectedRuntime, cluster); err != nil {
				l.Log().Fatalf("Failed to update environment of cluster '%s': %v", cluster.Name, err)
			}
			l.Log().Infof("Environment of cluster '%s' refreshed", cluster.Name)
		},
	}
	return cmd
}
