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
	"github.com/spf13/viper"

	cliutil "github.com/k3d-io/k3d/v5/cmd/util"
	cliconfig "github.com/k3d-io/k3d/v5/cmd/util/config"
	"github.com/k3d-io/k3d/v5/pkg/client"
	"github.com/k3d-io/k3d/v5/pkg/config"
	conf "github.com/k3d-io/k3d/v5/pkg/config/v1alpha5"
	l "github.com/k3d-io/k3d/v5/pkg/logger"
	"github.com/k3d-io/k3d/v5/pkg/runtimes"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

// NewCmdClusterReconfigure returns a new cobra command for `k3d cluster reconfigure`.
//
// Refs k3d-io/k3d#931 (rolling upgrade / reconfigure of running clusters).
func NewCmdClusterReconfigure() *cobra.Command {
	opts := client.ClusterReconfigureOpts{}
	var configFile string
	var dryRun bool

	// separate viper instance: the config file is only read by this command
	reconfigureCfgViper := viper.New()

	cmd := &cobra.Command{
		Use:   "reconfigure CLUSTER",
		Short: "[EXPERIMENTAL] Rolling reconfigure of an existing cluster (k3s image upgrade or config-file diff)",
		Long: `[EXPERIMENTAL] Replace cluster nodes one at a time with a modified spec.

Two modes:
  --image IMAGE   roll a new k3s image through the cluster (rolling upgrade)
  -c CONFIGFILE   diff a SimpleConfig file (same format as 'cluster create -c')
                  against the running cluster and replace exactly the nodes
                  whose spec differs (volumes, k3s args, env, k3s node
                  labels, memory, image). Serverlb port mappings (incl. the
                  kube-api host port) are applied by replacing only the
                  loadbalancer container. Unchanged nodes are not touched.
                  With --dry-run the per-node and per-port diff is printed
                  and nothing is changed.

Config-diff semantics: the config file describes the *desired* state of the
user-configurable node spec. An omitted image field means "keep the image
each node currently runs" (it does not reset to the default k3s image).
Applying the same file twice is a no-op; removing an entry (e.g. a volume
mount) from the file removes it from the nodes on the next apply. Changes that cannot be applied by replacing nodes
(network, subnet, token, node count)
abort with an error naming them; aspects that cannot be compared against a
running cluster (registries, hostAliases, runtime ulimits/labels, files,
gpus) are reported as not-diffed instead of being silently ignored.

Serverlb port-mapping changes (ports section, and an explicitly requested
kube-api host port via kubeAPI.hostPort) are applied by replacing only the
loadbalancer container — no node drain, no etcd rotation, and no --force
required, matching 'k3d cluster edit --port-add' which replaces the
loadbalancer the same way. Ingress traffic and API access through the
loadbalancer drop for a few seconds during the replacement; the control
plane itself keeps running. If the kube-api host port changed, the default
kubeconfig is updated automatically; externally saved kubeconfig copies
must be refreshed manually ('k3d kubeconfig get/merge' reflect the new
port). Without a loadbalancer (--no-lb clusters), port changes still
require a cluster recreate.

Replacement order: non-init servers, then init server, then agents — same
convention as kubeadm and OKD, so the kube version skew policy
(kubelet ≤ apiserver) is honored throughout the traversal.

Each server replacement performs a clean etcd member rotation in HA:
remove the old peer's membership via the etcd gRPC API, wipe its etcd
data, and let the new container join as a regular peer. This avoids the
"resume from stale bootstrap state" panic on the init server
(k3s-io/k3s#8148) and any IP-change-vs-peer-URL mismatch on non-init
servers — every server replacement is a clean rotation.

Replacing servers of a single-server cluster requires --force because it
temporarily makes the kube API unavailable. Agent-only changes do not.

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
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if configFile == "" {
				return nil
			}
			return cliconfig.InitViperWithConfigFile(reconfigureCfgViper, configFile)
		},
		Run: func(cmd *cobra.Command, args []string) {
			cluster, err := client.ClusterGet(cmd.Context(), runtimes.SelectedRuntime, &k3d.Cluster{Name: args[0]})
			if err != nil {
				l.Log().Fatalln(err)
			}

			if configFile == "" && opts.Image == "" {
				l.Log().Fatalln("Nothing to reconfigure: pass --image or a config file via -c")
			}
			if configFile != "" && opts.Image != "" {
				l.Log().Fatalln("--image and -c are mutually exclusive: put the image into the config file instead")
			}
			if dryRun && configFile == "" {
				l.Log().Fatalln("--dry-run requires a config file (-c)")
			}

			if configFile != "" {
				clusterConfig := computeReconfigureTargetConfig(cmd, args[0], reconfigureCfgViper, configFile)
				l.Log().Infof("Reconfiguring cluster '%s' from config '%s' (dry-run=%v force=%v drain-timeout=%s ready-timeout=%s)",
					cluster.Name, configFile, dryRun, opts.Force, opts.DrainTimeout, opts.ReadyTimeout)
				if err := client.ClusterReconfigureFromConfig(cmd.Context(), runtimes.SelectedRuntime, cluster, clusterConfig, dryRun, opts); err != nil {
					l.Log().Fatalf("Failed to reconfigure cluster '%s': %v", cluster.Name, err)
				}
				return
			}

			l.Log().Infof("Reconfiguring cluster '%s' (image=%q force=%v drain-timeout=%s ready-timeout=%s)",
				cluster.Name, opts.Image, opts.Force, opts.DrainTimeout, opts.ReadyTimeout)

			if err := client.ClusterReconfigure(cmd.Context(), runtimes.SelectedRuntime, cluster, opts); err != nil {
				l.Log().Fatalf("Failed to reconfigure cluster '%s': %v", cluster.Name, err)
			}
		},
	}

	cmd.Flags().StringVar(&opts.Image, "image", "", "New k3s image to roll out across server and agent nodes (e.g. rancher/k3s:v1.31.0-k3s1)")
	cmd.Flags().StringVarP(&configFile, "config", "c", "", "SimpleConfig file describing the desired cluster spec (same format as 'cluster create -c')")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Only print the per-node diff against the config file, change nothing")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "Allow replacing servers of a single-server cluster despite the resulting API downtime")
	cmd.Flags().DurationVar(&opts.DrainTimeout, "drain-timeout", 60*time.Second, "Per-node timeout for kubectl drain")
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", 120*time.Second, "Per-node timeout to become Ready after replacement")

	return cmd
}

// computeReconfigureTargetConfig loads and transforms a SimpleConfig file
// through the exact pipeline `cluster create` uses, so the desired per-node
// specs are derived identically. Fatals on any error (CLI context).
func computeReconfigureTargetConfig(cmd *cobra.Command, clusterName string, cfgViper *viper.Viper, configFile string) *conf.ClusterConfig {
	simpleCfg, err := config.SimpleConfigFromViper(cfgViper)
	if err != nil {
		l.Log().Fatalln(err)
	}

	if simpleCfg.Name != "" && simpleCfg.Name != clusterName {
		l.Log().Fatalf("Config file is for cluster '%s', but reconfiguring cluster '%s'", simpleCfg.Name, clusterName)
	}
	simpleCfg.Name = clusterName

	if err := config.ProcessSimpleConfig(&simpleCfg, runtimes.SelectedRuntime); err != nil {
		l.Log().Fatalf("error processing/sanitizing simple config: %v", err)
	}

	clusterConfig, err := config.TransformSimpleToClusterConfig(cmd.Context(), runtimes.SelectedRuntime, simpleCfg, configFile)
	if err != nil {
		l.Log().Fatalln(err)
	}

	clusterConfig, err = config.ProcessClusterConfig(*clusterConfig)
	if err != nil {
		l.Log().Fatalf("error processing cluster configuration: %v", err)
	}

	if err := config.ValidateClusterConfig(cmd.Context(), runtimes.SelectedRuntime, *clusterConfig); err != nil {
		l.Log().Fatalln("Failed Cluster Configuration Validation: ", err)
	}

	return clusterConfig
}
