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
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	conf "github.com/k3d-io/k3d/v5/pkg/config/v1alpha5"
	"github.com/k3d-io/k3d/v5/pkg/runtimes"
	"github.com/k3d-io/k3d/v5/pkg/types/k3s"
	"github.com/spf13/viper"
	"gotest.tools/assert"
)

func TestProcessClusterConfig(t *testing.T) {
	cfgFile := "./test_assets/config_test_simple.yaml"

	vip := viper.New()
	vip.SetConfigFile(cfgFile)
	_ = vip.ReadInConfig()

	cfg, err := FromViper(vip)
	if err != nil {
		t.Error(err)
	}

	t.Logf("\n========== Read Config and transform to cluster ==========\n%+v\n=================================\n", cfg)

	clusterCfg, err := TransformSimpleToClusterConfig(context.Background(), runtimes.Docker, cfg.(conf.SimpleConfig), cfgFile)
	if err != nil {
		t.Error(err)
	}

	// append some volume to test K3s volume shortcut expansion
	clusterCfg.Cluster.Nodes[0].Volumes = append(clusterCfg.Cluster.Nodes[0].Volumes, "/tmp/testexpansion:k3s-storage:rw")

	t.Logf("\n========== Process Cluster Config (non-host network) ==========\n%+v\n=================================\n", cfg)

	clusterCfg, err = ProcessClusterConfig(*clusterCfg)
	require.NoError(t, err)
	assert.Assert(t, clusterCfg.ClusterCreateOpts.DisableLoadBalancer == false, "The load balancer should be enabled")

	for _, v := range clusterCfg.Cluster.Nodes[0].Volumes {
		if strings.HasPrefix(v, "/tmp/testexpansion") {
			assert.Assert(t, strings.Contains(v, k3s.K3sPathStorage), "volume path shortcut expansion of k3s-storage didn't work")
		}
	}

	t.Logf("\n===== Resulting Cluster Config (non-host network) =====\n%+v\n===============\n", clusterCfg)

	t.Logf("\n========== Process Cluster Config (host network) ==========\n%+v\n=================================\n", cfg)

	clusterCfg.Cluster.Network.Name = "host"
	clusterCfg, err = ProcessClusterConfig(*clusterCfg)
	require.NoError(t, err)
	assert.Assert(t, clusterCfg.ClusterCreateOpts.DisableLoadBalancer == true, "The load balancer should be disabled")

	t.Logf("\n===== Resulting Cluster Config (host network) =====\n%+v\n===============\n", clusterCfg)
	t.Logf("\n===== First Node in Resulting Cluster Config (host network) =====\n%+v\n===============\n", clusterCfg.Cluster.Nodes[0])
}

func TestApplyAutoGPU(t *testing.T) {
	supportNone := func() nvidiaSupport { return nvidiaSupport{} }
	supportCDI := func() nvidiaSupport { return nvidiaSupport{CDI: true, NvidiaRuntime: true} }
	supportCDIOnly := func() nvidiaSupport { return nvidiaSupport{CDI: true} }
	supportRuntimeOnly := func() nvidiaSupport { return nvidiaSupport{NvidiaRuntime: true} }

	cases := []struct {
		name           string
		autoGPU        string
		gpuRequest     string // pre-set explicit --gpus
		devices        []string
		support        func() nvidiaSupport
		wantErr        bool
		wantGPURequest string
		wantDevices    []string
	}{
		{
			name:           "empty value is a no-op",
			autoGPU:        "",
			support:        supportNone,
			wantGPURequest: "",
			wantDevices:    nil,
		},
		{
			name:           "explicit none applies nothing",
			autoGPU:        "none",
			support:        supportNone,
			wantGPURequest: "",
			wantDevices:    nil,
		},
		{
			name:    "invalid vendor errors",
			autoGPU: "radeon",
			support: supportNone,
			wantErr: true,
		},
		{
			name:           "explicit --gpus wins, mapping skipped",
			autoGPU:        "nvidia",
			gpuRequest:     "device=0",
			support:        supportCDI,
			wantGPURequest: "device=0",
			wantDevices:    nil,
		},
		{
			name:           "explicit --device wins, mapping skipped",
			autoGPU:        "nvidia",
			devices:        []string{"/dev/foo"},
			support:        supportCDI,
			wantGPURequest: "",
			wantDevices:    []string{"/dev/foo"},
		},
		{
			// NVIDIA GPU present, but daemon has neither CDI nor the nvidia
			// runtime: mapping to --gpus all would fail hard at container
			// creation, so nothing must be applied.
			name:           "nvidia without toolkit skips passthrough",
			autoGPU:        "nvidia",
			support:        supportNone,
			wantGPURequest: "",
			wantDevices:    nil,
		},
		{
			name:           "nvidia with runtime but without CDI uses legacy --gpus all",
			autoGPU:        "nvidia",
			support:        supportRuntimeOnly,
			wantGPURequest: "all",
			wantDevices:    nil,
		},
		{
			name:           "nvidia with CDI uses device passthrough",
			autoGPU:        "nvidia",
			support:        supportCDIOnly,
			wantGPURequest: "",
			wantDevices:    []string{"nvidia.com/gpu=all"},
		},
		{
			name:           "intel does not consult nvidia support",
			autoGPU:        "intel",
			support:        nil, // would panic if called
			wantGPURequest: "",
			wantDevices:    []string{"/dev/dri:/dev/dri:rwm"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &conf.SimpleConfig{}
			cfg.Options.Runtime.AutoGPU = c.autoGPU
			cfg.Options.Runtime.GPURequest = c.gpuRequest
			cfg.Options.Runtime.Devices = c.devices

			err := applyAutoGPU(cfg, c.support)
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.wantGPURequest, cfg.Options.Runtime.GPURequest)
			require.Equal(t, c.wantDevices, cfg.Options.Runtime.Devices)
		})
	}
}

func TestApplyAutoGPUExplicitConfigSkipsDetection(t *testing.T) {
	// With explicit --gpus set, applyAutoGPU must return before running
	// ParseVendor (which for "auto" triggers host detection probes) and
	// before consulting the runtime for NVIDIA support. A nil support
	// probe would panic if the ordering regressed on the probe side.
	cfg := &conf.SimpleConfig{}
	cfg.Options.Runtime.AutoGPU = "auto"
	cfg.Options.Runtime.GPURequest = "all"

	require.NoError(t, applyAutoGPU(cfg, nil))
	require.Equal(t, "all", cfg.Options.Runtime.GPURequest)
	require.Empty(t, cfg.Options.Runtime.Devices)
}
