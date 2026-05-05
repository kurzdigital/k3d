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
	"time"

	"github.com/spf13/cobra"

	cliutil "github.com/k3d-io/k3d/v5/cmd/util"
	"github.com/k3d-io/k3d/v5/pkg/client"
	l "github.com/k3d-io/k3d/v5/pkg/logger"
	"github.com/k3d-io/k3d/v5/pkg/runtimes"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

// NewCmdClusterRollout returns a new cobra command for `k3d cluster rollout`.
func NewCmdClusterRollout() *cobra.Command {
	opts := client.ClusterRolloutOpts{}

	cmd := &cobra.Command{
		Use:   "rollout CLUSTER",
		Short: "[EXPERIMENTAL] Cycle a cluster's nodes one at a time (drain → stop+start → uncordon)",
		Long: `[EXPERIMENTAL] Cycle the nodes of an existing cluster sequentially.

Each node is drained (agents) or auto-drained via the k3d entrypoint
(servers), the container is stopped and started again, then waited on
until kubectl reports the node Ready before moving on. Analogous to
` + "`kubectl rollout restart deployment/X`" + ` but at the node level.

Containers are preserved across the rollout — only the k3s process and
its kubelet are replaced. State on /var/lib/rancher/k3s (etcd db, certs,
kubelet data) is therefore inherently retained.

NOT a substitute for ` + "`cluster restart`" + `: rollout does not re-run the
cluster-level post-start phase (HostAliases injection, CoreDNS configmap
rewrite). Use ` + "`cluster restart`" + ` after host-side changes such as a
WLAN switch, Docker daemon restart, or network recreate.

Single-server clusters require --force because cycling the only
control plane node temporarily makes the kube API unavailable.

Server graceful drain/uncordon relies on the k3d entrypoint, which is
only installed when a fix is active (K3D_FIX_*) at cluster creation.
If the server containers do not run through the entrypoint, they would
be cycled without draining — the rollout aborts unless --force is
given.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: cliutil.ValidArgsAvailableClusters,
		Run: func(cmd *cobra.Command, args []string) {
			cluster, err := client.ClusterGet(cmd.Context(), runtimes.SelectedRuntime, &k3d.Cluster{Name: args[0]})
			if err != nil {
				l.Log().Fatalln(err)
			}

			l.Log().Infof("Rolling out cluster '%s' (force=%v drain-timeout=%s ready-timeout=%s)",
				cluster.Name, opts.Force, opts.DrainTimeout, opts.ReadyTimeout)

			if err := client.ClusterRollout(cmd.Context(), runtimes.SelectedRuntime, cluster, opts); err != nil {
				l.Log().Fatalf("Failed to roll out cluster '%s': %v", cluster.Name, err)
			}
		},
	}

	cmd.Flags().BoolVar(&opts.Force, "force", false, "Allow rollout on a single-server cluster despite the resulting API downtime")
	cmd.Flags().DurationVar(&opts.DrainTimeout, "drain-timeout", 60*time.Second, "Per-agent timeout for kubectl drain")
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", 120*time.Second, "Per-node timeout to become Ready after restart")

	return cmd
}
