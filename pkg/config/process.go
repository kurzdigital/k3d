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

package config

import (
	"os"
	"strings"

	conf "github.com/k3d-io/k3d/v5/pkg/config/v1alpha5"
	l "github.com/k3d-io/k3d/v5/pkg/logger"
	k3drt "github.com/k3d-io/k3d/v5/pkg/runtimes"
	runtimeutil "github.com/k3d-io/k3d/v5/pkg/runtimes/util"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
	"github.com/k3d-io/k3d/v5/pkg/types/fixes"
	"github.com/k3d-io/k3d/v5/pkg/types/k3s"
	"github.com/k3d-io/k3d/v5/pkg/util/gpu"
)

// ProcessSimpleConfig applies processing to the simple config, sanitizing it and doing some modifications.
//
// The runtime is passed in so auto-GPU detection can ask it (via the abstract
// Runtime interface) whether NVIDIA passthrough is available (CDI device
// discovered or nvidia runtime registered), instead of reaching into a
// concrete runtime implementation from this layer.
func ProcessSimpleConfig(simpleConfig *conf.SimpleConfig, runtime k3drt.Runtime) error {
	if simpleConfig.Network == "host" {
		l.Log().Infoln("[SimpleConfig] Hostnetwork selected - disabling injection of docker host into the cluster, server load balancer and setting the api port to the k3s default")
		simpleConfig.Options.K3dOptions.DisableLoadbalancer = true

		l.Log().Debugf("Host network was chosen, changing provided/random api port to k3s:%s", k3d.DefaultAPIPort)
		simpleConfig.ExposeAPI.HostPort = k3d.DefaultAPIPort

		l.Log().Debugln("Host network was chosen, disabling DNS fix as no gateway will be available/required.")
		err := os.Setenv(string(fixes.EnvFixDNS), "false")
		if err != nil {
			return err
		}
	}

	if err := applyAutoGPU(simpleConfig, runtimeNvidiaSupport(runtime)); err != nil {
		return err
	}

	return nil
}

// nvidiaSupport describes what the container runtime offers for NVIDIA GPU
// passthrough. Zero value means "nothing available".
type nvidiaSupport struct {
	CDI           bool // an `nvidia.com/gpu` CDI device is discovered
	NvidiaRuntime bool // an "nvidia" runtime is registered with the daemon (or is its default)
}

// runtimeNvidiaSupport builds the NVIDIA-support probe for applyAutoGPU from
// the abstract runtime, keeping the concrete-runtime knowledge (RuntimeInfo)
// out of the branching logic so the latter stays unit-testable without a
// daemon.
func runtimeNvidiaSupport(runtime k3drt.Runtime) func() nvidiaSupport {
	return func() nvidiaSupport {
		if runtime == nil {
			return nvidiaSupport{}
		}
		info, err := runtime.Info()
		if err != nil {
			l.Log().Debugf("[auto-gpu] could not query runtime info for NVIDIA support detection: %v", err)
			return nvidiaSupport{}
		}
		return nvidiaSupport{
			CDI:           info.HasNvidiaCDI,
			NvidiaRuntime: info.HasNvidiaRuntime,
		}
	}
}

// applyAutoGPU resolves the --auto-gpu flag (if set) to a vendor and merges
// the resulting GPURequest / Devices into simpleConfig.Options.Runtime,
// without overwriting values the user already supplied explicitly. This way
// the rest of the pipeline doesn't need to know about auto-detection.
//
// queryNvidiaSupport is injected (rather than queried inline) so the branching
// can be tested without a live container runtime.
func applyAutoGPU(simpleConfig *conf.SimpleConfig, queryNvidiaSupport func() nvidiaSupport) error {
	if simpleConfig.Options.Runtime.AutoGPU == "" {
		return nil
	}

	r := &simpleConfig.Options.Runtime
	// Explicit user config wins over the whole auto-detection: if the user
	// already supplied --gpus or --device, we don't second-guess them by
	// mixing in detected mappings (which could produce nonsense like
	// NVIDIA --gpus combined with AMD /dev/kfd). Checked BEFORE ParseVendor
	// so `--auto-gpu auto` doesn't run (potentially slow) detection probes
	// whose result would be thrown away anyway.
	if r.GPURequest != "" || len(r.Devices) > 0 {
		l.Log().Infoln("[auto-gpu] Explicit --gpus / --device already set; skipping auto-detection mapping.")
		return nil
	}

	vendor, err := gpu.ParseVendor(simpleConfig.Options.Runtime.AutoGPU)
	if err != nil {
		return err
	}
	switch vendor {
	case gpu.VendorNone:
		l.Log().Infoln("[auto-gpu] No GPU detected on this host. No device passthrough applied.")
		return nil
	case gpu.VendorUnknown:
		l.Log().Warnln("[auto-gpu] WSL2 GPU passthrough is configured (/dev/dxg present) but vendor detection failed. Install vulkan-tools or mesa-utils, or pass --auto-gpu=<nvidia|amd|intel> explicitly.")
		return nil
	}

	cdi := false
	if vendor == gpu.VendorNVIDIA {
		support := queryNvidiaSupport()
		// A detected NVIDIA GPU alone is not enough: mapping to `--gpus all`
		// on a daemon without the NVIDIA Container Toolkit would make
		// container creation fail hard. Require either CDI or a registered
		// "nvidia" runtime before applying any NVIDIA mapping.
		if !support.CDI && !support.NvidiaRuntime {
			l.Log().Warnln("[auto-gpu] NVIDIA GPU detected but neither CDI nor the nvidia container runtime is available on the container runtime daemon — skipping GPU passthrough. Install and configure the NVIDIA Container Toolkit, or pass explicit --gpus/--device flags.")
			return nil
		}
		cdi = support.CDI
	}

	mapping := gpu.MapVendor(vendor, cdi)
	r.GPURequest = mapping.GPURequest
	r.Devices = append(r.Devices, mapping.Devices...)
	if mapping.Notes != "" {
		l.Log().Infof("[auto-gpu] %s", mapping.Notes)
	}
	return nil
}

// ProcessClusterConfig applies processing to the config sanitizing it and doing
// some final modifications
func ProcessClusterConfig(clusterConfig conf.ClusterConfig) (*conf.ClusterConfig, error) {
	cluster := clusterConfig.Cluster
	if cluster.Network.Name == "host" {
		l.Log().Infoln("[ClusterConfig] Hostnetwork selected - disabling injection of docker host into the cluster, server load balancer and setting the api port to the k3s default")
		// if network is set to host, exposed api port must be the one imposed by k3s
		k3sPort := cluster.KubeAPI.Port.Port()
		l.Log().Debugf("Host network was chosen, changing provided/random api port to k3s:%s", k3sPort)
		cluster.KubeAPI.PortMapping.Binding.HostPort = k3sPort

		// if network is host, disable load balancer
		// serverlb not supported in hostnetwork mode due to port collisions with server node
		clusterConfig.ClusterCreateOpts.DisableLoadBalancer = true
	}

	for _, node := range clusterConfig.Cluster.Nodes {
		for vIndex, volume := range node.Volumes {
			_, dest, err := runtimeutil.ReadVolumeMount(volume)
			if err != nil {
				return nil, err
			}
			if path, ok := k3s.K3sPathShortcuts[dest]; ok {
				l.Log().Tracef("[node: %s] expanding volume shortcut %s to %s", node.Name, dest, path)
				node.Volumes[vIndex] = strings.Replace(volume, dest, path, 1)
			}
		}
	}

	return &clusterConfig, nil
}
