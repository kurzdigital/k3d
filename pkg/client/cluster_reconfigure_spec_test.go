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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	conf "github.com/k3d-io/k3d/v5/pkg/config/v1alpha5"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

const testAnonVolume = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// reconstructedServer builds a node the way ClusterGet would reconstruct a
// server container that was created from the given user-scope spec — all
// k3d-managed injections included.
func reconstructedServer(name string, userArgs, userEnv, userVolumes []string, nodeLabels map[string]string) *k3d.Node {
	cmd := []string{"server"}
	cmd = append(cmd, userArgs...)
	for k, v := range nodeLabels {
		cmd = append(cmd, "--node-label", k+"="+v)
	}
	cmd = append(cmd, "--tls-san", "0.0.0.0", "--tls-san", "k3d-test-serverlb")

	env := []string{"PATH=/usr/bin", "CRI_CONFIG_FILE=/var/lib/rancher/k3s/agent/etc/crictl.yaml"}
	env = append(env, userEnv...)
	env = append(env, "K3S_TOKEN=secret-token", "K3S_KUBECONFIG_OUTPUT=/output/kubeconfig.yaml")

	volumes := append([]string{}, userVolumes...)
	volumes = append(volumes,
		"k3d-test-images:/k3d/images",
		testAnonVolume+":/var/lib/rancher/k3s",
	)

	return &k3d.Node{
		Name:    name,
		Role:    k3d.ServerRole,
		Image:   "docker.io/rancher/k3s:v1.31.0-k3s1",
		Cmd:     cmd,
		Args:    []string{},
		Env:     env,
		Volumes: volumes,
		RuntimeLabels: map[string]string{
			k3d.LabelServerAPIHost:      "0.0.0.0",
			k3d.LabelServerLoadBalancer: "k3d-test-serverlb",
			k3d.LabelClusterName:        "test",
		},
	}
}

// targetNode builds a node the way TransformSimpleToClusterConfig produces
// it: user-scope fields only.
func targetNode(name string, role k3d.Role, image string, args, env, volumes []string, nodeLabels map[string]string) *k3d.Node {
	return &k3d.Node{
		Name:          name,
		Role:          role,
		Image:         image,
		Args:          args,
		Env:           env,
		Volumes:       volumes,
		K3sNodeLabels: nodeLabels,
	}
}

func TestUserScopeEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"CRI_CONFIG_FILE=/x",
		"K3S_TOKEN=abc",
		"K3S_URL=https://server:6443",
		"K3S_KUBECONFIG_OUTPUT=/output/kubeconfig.yaml",
		"K3D_SOMETHING=internal",
		"FOO=bar",
		"K3S_DATASTORE_ENDPOINT=mysql://x", // user-scope: not on the denylist
	}
	assert.Equal(t, []string{"FOO=bar", "K3S_DATASTORE_ENDPOINT=mysql://x"}, userScopeEnv(in))
	assert.Equal(t, []string{"K3S_TOKEN=abc", "K3S_URL=https://server:6443"}, preservedEnv(in))
}

func TestVolumeScoping(t *testing.T) {
	volumes := []string{
		"/host/data:/data",
		"k3d-test-images:/k3d/images",
		"/home/user/.k3d/fake/meminfo:/proc/meminfo:ro",
		"/home/user/.k3d/fake/edac:/sys/devices/system/edac:ro",
		testAnonVolume + ":/var/lib/rancher/k3s",
		"named-vol:/var/something",
	}
	assert.Equal(t, []string{"/host/data:/data", "named-vol:/var/something"}, userScopeVolumes(volumes))
	// managed complement keeps image volume + adopted anonymous volume,
	// drops the regenerated meminfo/edac fakes
	assert.Equal(t, []string{"k3d-test-images:/k3d/images", testAnonVolume + ":/var/lib/rancher/k3s"}, managedVolumes(volumes))
}

func TestUserScopeArgs(t *testing.T) {
	node := reconstructedServer("k3d-test-server-0",
		[]string{"--kube-apiserver-arg=v=2", "--tls-san", "my.custom.san"},
		nil, nil,
		map[string]string{"tier": "control"},
	)
	// simulate an older cluster that also carries --cluster-init and a
	// duplicated injected tls-san pair (historical replace behavior)
	node.Cmd = append(node.Cmd, "--cluster-init", "--tls-san", "0.0.0.0")

	args, labels := userScopeArgs(node)
	assert.Equal(t, []string{"--kube-apiserver-arg=v=2", "--tls-san", "my.custom.san"}, args)
	assert.Equal(t, map[string]string{"tier": "control"}, labels)
}

func TestNormalizers(t *testing.T) {
	assert.Equal(t, normalizeImageRef("rancher/k3s:v1.31.0-k3s1"), normalizeImageRef("docker.io/rancher/k3s:v1.31.0-k3s1"))
	assert.Equal(t, normalizeMemory("1g"), normalizeMemory("1024m"))
	assert.NotEqual(t, normalizeMemory("1g"), normalizeMemory("2g"))
	assert.Equal(t, "", normalizeMemory(""))
}

func TestDiffNodeSpecUnchanged(t *testing.T) {
	actual := reconstructedServer("k3d-test-server-0",
		[]string{"--kube-apiserver-arg=v=2"},
		[]string{"FOO=bar"},
		[]string{"/host/data:/data"},
		map[string]string{"tier": "control"},
	)
	target := targetNode("k3d-test-server-0", k3d.ServerRole, "rancher/k3s:v1.31.0-k3s1",
		[]string{"--kube-apiserver-arg=v=2"},
		[]string{"FOO=bar"},
		[]string{"/host/data:/data"},
		map[string]string{"tier": "control"},
	)

	diff := diffNodeSpec(target, actual)
	assert.Empty(t, diff.Changes, "identical user-scope specs must not produce changes: %+v", diff.Changes)
	assert.Nil(t, diff.Changeset)
}

func TestDiffNodeSpecChanges(t *testing.T) {
	actual := reconstructedServer("k3d-test-server-0",
		[]string{"--kube-apiserver-arg=v=2"},
		[]string{"FOO=bar"},
		[]string{"/host/data:/data"},
		nil,
	)

	target := targetNode("k3d-test-server-0", k3d.ServerRole, "rancher/k3s:v1.32.0-k3s1", // image changed
		[]string{"--kube-apiserver-arg=v=2", "--kube-apiserver-arg=audit-log-path=/tmp/audit"}, // arg added
		nil,                              // env FOO=bar revoked
		[]string{"/host/other:/data2"},   // volume replaced
		map[string]string{"role": "new"}, // label added
	)

	diff := diffNodeSpec(target, actual)
	fields := map[string]bool{}
	for _, ch := range diff.Changes {
		fields[ch.Field] = true
	}
	assert.True(t, fields["image"], "image change not detected")
	assert.True(t, fields["k3sArgs"], "arg change not detected")
	assert.True(t, fields["env"], "env revocation not detected")
	assert.True(t, fields["volumes"], "volume change not detected")
	assert.True(t, fields["k3sNodeLabels"], "label change not detected")
	assert.False(t, fields["memory"], "memory falsely detected")
	require.NotNil(t, diff.Changeset)
	assert.Equal(t, target.Volumes, diff.Changeset.Volumes)
	assert.Equal(t, target.Args, diff.Changeset.Args)
}

// TestApplySpecRoundtrip is the idempotency core: applying a changeset to a
// reconstructed node, re-running the create-time injections, and
// re-extracting the user scope must yield exactly the desired spec.
func TestApplySpecRoundtrip(t *testing.T) {
	actual := reconstructedServer("k3d-test-server-0",
		[]string{"--old-arg=1"},
		[]string{"OLD=env"},
		[]string{"/old:/mount"},
		map[string]string{"old": "label"},
	)

	spec := &NodeSpecChangeset{
		Image:         "rancher/k3s:v1.32.0-k3s1",
		Volumes:       []string{"/new:/mount"},
		Args:          []string{"--new-arg=2"},
		Env:           []string{"NEW=env"},
		K3sNodeLabels: map[string]string{"new": "label"},
		Memory:        "",
	}

	result, err := CopyNode(context.Background(), actual, CopyNodeOpts{keepState: false})
	require.NoError(t, err)
	applySpecChangeset(nil, result, spec)

	// old user volumes gone, managed mounts (image volume, adopted
	// anonymous volume) preserved, new user volume present
	assert.NotContains(t, result.Volumes, "/old:/mount")
	assert.Contains(t, result.Volumes, "/new:/mount")
	assert.Contains(t, result.Volumes, "k3d-test-images:/k3d/images")
	assert.Contains(t, result.Volumes, testAnonVolume+":/var/lib/rancher/k3s")

	// env: join state preserved, old user env revoked, new present
	assert.Contains(t, result.Env, "K3S_TOKEN=secret-token")
	assert.NotContains(t, result.Env, "OLD=env")
	assert.Contains(t, result.Env, "NEW=env")

	// cmd rebuilt: NodeCreate will re-inject role token / tls-san / labels
	assert.Nil(t, result.Cmd)
	assert.Equal(t, []string{"--new-arg=2"}, result.Args)

	// simulate the create-time injections (NodeCreate + patchServerSpec)
	for k, v := range result.K3sNodeLabels {
		result.Args = append(result.Args, "--node-label", k+"="+v)
	}
	result.Cmd = []string{"server"}
	result.Args = append(result.Args, "--tls-san", result.RuntimeLabels[k3d.LabelServerAPIHost], "--tls-san", result.RuntimeLabels[k3d.LabelServerLoadBalancer])
	result.Env = append(result.Env, "K3S_KUBECONFIG_OUTPUT=/output/kubeconfig.yaml")

	// re-extract: must equal the desired spec exactly
	args, labels := userScopeArgs(result)
	assert.Equal(t, spec.Args, args)
	assert.Equal(t, spec.K3sNodeLabels, labels)
	assert.Equal(t, spec.Env, userScopeEnv(result.Env))
	assert.Equal(t, spec.Volumes, userScopeVolumes(result.Volumes))

	rediff := diffNodeSpec(&k3d.Node{
		Name:          "k3d-test-server-0",
		Role:          k3d.ServerRole,
		Image:         spec.Image,
		Args:          spec.Args,
		Env:           spec.Env,
		Volumes:       spec.Volumes,
		K3sNodeLabels: spec.K3sNodeLabels,
	}, result)
	assert.Empty(t, rediff.Changes, "re-diff after apply must be empty: %+v", rediff.Changes)
}

func TestDiffClusterSpecClusterLevel(t *testing.T) {
	actual := &k3d.Cluster{
		Name:    "test",
		Network: k3d.ClusterNetwork{Name: "k3d-test"},
		Nodes: []*k3d.Node{
			reconstructedServer("k3d-test-server-0", nil, nil, nil, nil),
		},
	}

	t.Run("name mismatch errors", func(t *testing.T) {
		cfg := &conf.ClusterConfig{Cluster: k3d.Cluster{Name: "other"}}
		_, err := DiffClusterSpec(context.Background(), nil, actual, cfg)
		require.Error(t, err)
	})

	t.Run("node count mismatch is unsupported", func(t *testing.T) {
		cfg := &conf.ClusterConfig{Cluster: k3d.Cluster{
			Name:    "test",
			Network: k3d.ClusterNetwork{Name: "k3d-test"},
			Nodes: []*k3d.Node{
				targetNode("k3d-test-server-0", k3d.ServerRole, "rancher/k3s:v1.31.0-k3s1", nil, nil, nil, nil),
				targetNode("k3d-test-agent-0", k3d.AgentRole, "rancher/k3s:v1.31.0-k3s1", nil, nil, nil, nil),
			},
		}}
		diff, err := DiffClusterSpec(context.Background(), nil, actual, cfg)
		require.NoError(t, err)
		require.Len(t, diff.Unsupported, 1)
		assert.Contains(t, diff.Unsupported[0], "node count")
	})

	t.Run("network mismatch is unsupported", func(t *testing.T) {
		cfg := &conf.ClusterConfig{Cluster: k3d.Cluster{
			Name:    "test",
			Network: k3d.ClusterNetwork{Name: "other-net", External: true},
			Nodes: []*k3d.Node{
				targetNode("k3d-test-server-0", k3d.ServerRole, "rancher/k3s:v1.31.0-k3s1", nil, nil, nil, nil),
			},
		}}
		diff, err := DiffClusterSpec(context.Background(), nil, actual, cfg)
		require.NoError(t, err)
		require.NotEmpty(t, diff.Unsupported)
		assert.Contains(t, diff.Unsupported[0], "network")
	})

	t.Run("matching config yields unchanged node and not-diffable notes", func(t *testing.T) {
		cfg := &conf.ClusterConfig{
			Cluster: k3d.Cluster{
				Name:    "test",
				Network: k3d.ClusterNetwork{Name: "k3d-test"},
				Nodes: []*k3d.Node{
					targetNode("k3d-test-server-0", k3d.ServerRole, "rancher/k3s:v1.31.0-k3s1", nil, nil, nil, nil),
				},
			},
		}
		cfg.ClusterCreateOpts.HostAliases = []k3d.HostAlias{{IP: "1.2.3.4", Hostnames: []string{"x"}}}
		diff, err := DiffClusterSpec(context.Background(), nil, actual, cfg)
		require.NoError(t, err)
		assert.Empty(t, diff.Unsupported)
		assert.Empty(t, diff.ChangedNodes)
		assert.Equal(t, []string{"k3d-test-server-0"}, diff.UnchangedNodes)
		require.NotEmpty(t, diff.NotDiffable)
		assert.Contains(t, diff.NotDiffable[0], "hostAliases")
	})
}
