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

// NewCmdClusterReconfigure returns a new cobra command for `k3d cluster reconfigure`.
//
// Refs k3d-io/k3d#931 (rolling upgrade / reconfigure of running clusters).
func NewCmdClusterReconfigure() *cobra.Command {
	opts := client.ClusterReconfigureOpts{}

	cmd := &cobra.Command{
		Use:   "reconfigure CLUSTER",
		Short: "[EXPERIMENTAL] Rolling reconfigure of an existing cluster (e.g. k3s image upgrade)",
		Long: `[EXPERIMENTAL] Replace cluster nodes one at a time with a modified spec.

Currently supports changing the k3s image only (rolling upgrade). Order:
non-init servers, then init server, then agents — same convention as
kubeadm and OKD, so the kube version skew policy (kubelet ≤ apiserver) is
honored throughout the traversal.

Each server replacement performs a clean etcd member rotation in HA:
remove the old peer's membership via the etcd gRPC API, wipe its etcd
data, and let the new container join as a regular peer. This avoids the
"resume from stale bootstrap state" panic on the init server
(k3s-io/k3s#8148) and any IP-change-vs-peer-URL mismatch on non-init
servers — every server replacement is a clean rotation.

Single-server clusters require --force because replacing the only
control plane node temporarily makes the kube API unavailable.

WARNING: this operation is NOT automatically resumable. Each server's
etcd member removal and data wipe is irreversible, and interrupting the
run mid-upgrade (Ctrl-C, crash, timeout) can leave the cluster with a
half-rotated etcd membership and a partially upgraded node set. Recovery
in that state is manual (re-adding/removing etcd members, restoring or
re-creating affected nodes). Take a backup before running.

LIMITATION: HA etcd member rotation connects to etcd (port 2379) on the
container network from the machine running k3d. On Docker Desktop
(macOS/Windows) or with a remote DOCKER_HOST, container IPs are not
routable from the client, so reconfiguring multi-server clusters is not
supported there yet. Single-server clusters (--force) are unaffected.`,
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

			if opts.Image == "" {
				l.Log().Fatalln("Nothing to reconfigure: pass at least --image")
			}

			l.Log().Infof("Reconfiguring cluster '%s' (image=%q force=%v drain-timeout=%s ready-timeout=%s)",
				cluster.Name, opts.Image, opts.Force, opts.DrainTimeout, opts.ReadyTimeout)

			if err := client.ClusterReconfigure(cmd.Context(), runtimes.SelectedRuntime, cluster, opts); err != nil {
				l.Log().Fatalf("Failed to reconfigure cluster '%s': %v", cluster.Name, err)
			}
		},
	}

	cmd.Flags().StringVar(&opts.Image, "image", "", "New k3s image to roll out across server and agent nodes (e.g. rancher/k3s:v1.31.0-k3s1)")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "Allow reconfigure on a single-server cluster despite the resulting API downtime")
	cmd.Flags().DurationVar(&opts.DrainTimeout, "drain-timeout", 60*time.Second, "Per-node timeout for kubectl drain")
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", 120*time.Second, "Per-node timeout to become Ready after replacement")

	return cmd
}
