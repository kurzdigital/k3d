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

package docker

import (
	"os"
	"strconv"
	"testing"

	"github.com/go-test/deep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

func TestTranslateNodeToContainer(t *testing.T) {
	inputNode := &k3d.Node{
		Name:    "test",
		Role:    k3d.ServerRole,
		Image:   "rancher/k3s:v0.9.0",
		Volumes: []string{"/test:/tmp/test"},
		Env:     []string{"TEST_KEY_1=TEST_VAL_1"},
		Cmd:     []string{"server", "--https-listen-port=6443"},
		Args:    []string{"--some-boolflag"},
		Ports: nat.PortMap{
			"6443/tcp": []nat.PortBinding{
				{
					HostIP:   "0.0.0.0",
					HostPort: "6443",
				},
			},
		},
		Restart:       true,
		RuntimeLabels: map[string]string{k3d.LabelRole: string(k3d.ServerRole), "test_key_1": "test_val_1"},
		Networks:      []string{"mynet"},
	}

	init := true
	if disableInit, err := strconv.ParseBool(os.Getenv(k3d.K3dEnvDebugDisableDockerInit)); err == nil && disableInit {
		init = false
	}

	expectedRepresentation := &NodeInDocker{
		ContainerConfig: container.Config{
			Hostname: "test",
			Image:    "rancher/k3s:v0.9.0",
			Env:      []string{"TEST_KEY_1=TEST_VAL_1"},
			Cmd:      []string{"server", "--https-listen-port=6443", "--some-boolflag"},
			Labels:   map[string]string{k3d.LabelRole: string(k3d.ServerRole), "test_key_1": "test_val_1"},
			ExposedPorts: nat.PortSet{
				"6443/tcp": struct{}{},
			},
		},
		HostConfig: container.HostConfig{
			Binds:       []string{"/test:/tmp/test"},
			NetworkMode: "bridge",
			RestartPolicy: container.RestartPolicy{
				Name: "unless-stopped",
			},
			Init:       &init,
			Privileged: true,
			UsernsMode: "host",
			Tmpfs:      map[string]string{"/run": "", "/var/run": ""},
			PortBindings: nat.PortMap{
				"6443/tcp": {
					{
						HostIP:   "0.0.0.0",
						HostPort: "6443",
					},
				},
			},
		},
		NetworkingConfig: network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				"mynet": {},
			},
		},
	}

	actualRepresentation, err := TranslateNodeToContainer(inputNode)
	if err != nil {
		t.Error(err)
	}

	actualRepresentation.ContainerConfig.Entrypoint = expectedRepresentation.ContainerConfig.Entrypoint // may change depending on the enabled fixes, so we ignore it here

	if diff := deep.Equal(actualRepresentation, expectedRepresentation); diff != nil {
		t.Errorf("Actual representation\n%+v\ndoes not match expected representation\n%+v\nDiff:\n%+v", actualRepresentation, expectedRepresentation, diff)
	}
}

func TestParsePathDeviceMapping(t *testing.T) {
	cases := []struct {
		spec    string
		want    container.DeviceMapping
		isPath  bool
		wantErr bool
	}{
		{
			spec:   "/dev/nvidia0",
			want:   container.DeviceMapping{PathOnHost: "/dev/nvidia0", PathInContainer: "/dev/nvidia0", CgroupPermissions: "rwm"},
			isPath: true,
		},
		{
			spec:   "/dev/nvidia0:/dev/nvidia0",
			want:   container.DeviceMapping{PathOnHost: "/dev/nvidia0", PathInContainer: "/dev/nvidia0", CgroupPermissions: "rwm"},
			isPath: true,
		},
		{
			spec:   "/dev/nvidia0:/dev/foo",
			want:   container.DeviceMapping{PathOnHost: "/dev/nvidia0", PathInContainer: "/dev/foo", CgroupPermissions: "rwm"},
			isPath: true,
		},
		{
			spec:   "/dev/nvidia0:/dev/foo:r",
			want:   container.DeviceMapping{PathOnHost: "/dev/nvidia0", PathInContainer: "/dev/foo", CgroupPermissions: "r"},
			isPath: true,
		},
		// Empty container path falls back to host path.
		{
			spec:   "/dev/kvm::rw",
			want:   container.DeviceMapping{PathOnHost: "/dev/kvm", PathInContainer: "/dev/kvm", CgroupPermissions: "rw"},
			isPath: true,
		},
		// CDI specs are not paths and must be rejected here for the caller
		// to route them to DeviceRequests.
		{spec: "nvidia.com/gpu=all", isPath: false},
		{spec: "vendor.example/foo=bar", isPath: false},
		// Garbage that's not a path: also not a path.
		{spec: "all", isPath: false},
		{spec: "", isPath: false},
		// Malformed cgroup permissions must be rejected client-side
		// (docker CLI behaviour) instead of surfacing a daemon error.
		{spec: "/dev/nvidia0:/dev/nvidia0:rx", isPath: true, wantErr: true},
		{spec: "/dev/nvidia0:/dev/nvidia0:rr", isPath: true, wantErr: true},
		{spec: "/dev/nvidia0:/dev/nvidia0:rwx", isPath: true, wantErr: true},
	}

	for _, c := range cases {
		got, isPath, err := parsePathDeviceMapping(c.spec)
		if isPath != c.isPath {
			t.Errorf("spec %q: isPath = %v, want %v", c.spec, isPath, c.isPath)
			continue
		}
		if c.wantErr {
			if err == nil {
				t.Errorf("spec %q: expected an error, got none", c.spec)
			}
			continue
		}
		if err != nil {
			t.Errorf("spec %q: unexpected error: %v", c.spec, err)
			continue
		}
		if !c.isPath {
			continue
		}
		if got != c.want {
			t.Errorf("spec %q: got %+v, want %+v", c.spec, got, c.want)
		}
	}
}

func TestTranslateNodeToContainerInvalidDevice(t *testing.T) {
	cases := []struct {
		name    string
		device  string
		wantErr bool
	}{
		{name: "valid host path", device: "/dev/nvidia0", wantErr: false},
		{name: "valid CDI spec", device: "nvidia.com/gpu=all", wantErr: false},
		// Neither a path nor a well-formed CDI ID -> must be rejected.
		{name: "garbage", device: "all", wantErr: true},
		{name: "missing equals", device: "nvidia.com/gpu", wantErr: true},
		{name: "missing slash", device: "gpu=all", wantErr: true},
		// Path-style spec with bad cgroup permissions -> rejected client-side.
		{name: "invalid cgroup permissions", device: "/dev/nvidia0:/dev/nvidia0:rx", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inputNode := &k3d.Node{
				Name:          "test",
				Role:          k3d.ServerRole,
				Image:         "rancher/k3s:v0.9.0",
				RuntimeLabels: map[string]string{k3d.LabelRole: string(k3d.ServerRole)},
				Networks:      []string{"mynet"},
				Devices:       []string{c.device},
			}

			_, err := TranslateNodeToContainer(inputNode)
			if c.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestTranslateContainerDetailsToNodeVolumes covers the volume/mount surfacing
// logic: explicit binds are kept verbatim, anonymous (named) volumes from
// containerDetails.Mounts are turned into pseudo-binds, Mounts entries that
// duplicate a bind destination are dropped, and the optional Mode suffix is
// appended.
func TestTranslateContainerDetailsToNodeVolumes(t *testing.T) {
	testcases := []struct {
		name     string
		binds    []string
		mounts   []container.MountPoint
		expected []string
	}{
		{
			name:     "only explicit binds, no mounts",
			binds:    []string{"/host:/tmp/test"},
			mounts:   nil,
			expected: []string{"/host:/tmp/test"},
		},
		{
			name:  "anonymous volume surfaced as pseudo-bind",
			binds: nil,
			mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "anon-vol", Destination: "/var/lib/rancher/k3s"},
			},
			expected: []string{"anon-vol:/var/lib/rancher/k3s"},
		},
		{
			name:  "anonymous volume with mode suffix",
			binds: nil,
			mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "anon-vol", Destination: "/var/lib/rancher/k3s", Mode: "z"},
			},
			expected: []string{"anon-vol:/var/lib/rancher/k3s:z"},
		},
		{
			name:  "mount duplicating a bind destination is deduped",
			binds: []string{"/host/data:/var/lib/rancher/k3s"},
			mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "anon-vol", Destination: "/var/lib/rancher/k3s"},
			},
			expected: []string{"/host/data:/var/lib/rancher/k3s"},
		},
		{
			name:  "non-volume mount types are ignored",
			binds: nil,
			mounts: []container.MountPoint{
				{Type: mount.TypeBind, Name: "", Destination: "/somebind"},
				{Type: mount.TypeTmpfs, Name: "", Destination: "/run"},
				{Type: mount.TypeVolume, Name: "", Destination: "/no-name"},
			},
			expected: []string{},
		},
		{
			name:  "explicit binds kept and anonymous volume appended",
			binds: []string{"/host:/tmp/test"},
			mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "anon-vol", Destination: "/var/lib/rancher/k3s"},
			},
			expected: []string{"/host:/tmp/test", "anon-vol:/var/lib/rancher/k3s"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			details := newMinimalContainerDetails()
			details.HostConfig.Binds = tc.binds
			details.Mounts = tc.mounts

			node, err := TranslateContainerDetailsToNode(details)
			assert.NoError(t, err)
			assert.Equal(t, tc.expected, node.Volumes)
		})
	}
}

// newMinimalContainerDetails builds a ContainerJSON that passes the default
// label check in TranslateContainerDetailsToNode and is in a non-running state,
// so the volume/mount logic can be exercised in isolation without triggering
// network/IP parsing.

// newMinimalContainerDetails builds a ContainerJSON that passes the default
// label check in TranslateContainerDetailsToNode and is in a non-running state,
// so the volume/mount logic can be exercised in isolation without triggering
// network/IP parsing.
func newMinimalContainerDetails() dockertypes.ContainerJSON {
	return dockertypes.ContainerJSON{
		ContainerJSONBase: &container.ContainerJSONBase{
			Name: "/k3d-test-server-0",
			State: &container.State{
				Running: false,
				Status:  "exited",
			},
			HostConfig: &container.HostConfig{},
		},
		Config: &container.Config{
			Labels: map[string]string{
				"app":         "k3d",
				k3d.LabelRole: string(k3d.ServerRole),
			},
		},
		NetworkSettings: &container.NetworkSettings{},
	}
}

func TestTranslateNodeToContainerDockerRuntime(t *testing.T) {
	inputNode := &k3d.Node{
		Name:          "test",
		Role:          k3d.ServerRole,
		Image:         "rancher/k3s:v0.9.0",
		RuntimeLabels: map[string]string{k3d.LabelRole: string(k3d.ServerRole)},
		Networks:      []string{"mynet"},
		DockerRuntime: "nvidia",
	}
	inputNode.FillRuntimeLabels()

	repr, err := TranslateNodeToContainer(inputNode)
	require.NoError(t, err)
	assert.Equal(t, "nvidia", repr.HostConfig.Runtime, "node.DockerRuntime must propagate to HostConfig.Runtime")
	assert.Equal(t, "nvidia", repr.ContainerConfig.Labels[k3d.LabelNodeDockerRuntime], "an explicit DockerRuntime must be persisted as a container label")
}

func TestTranslateNodeToContainerEmptyDockerRuntime(t *testing.T) {
	inputNode := &k3d.Node{
		Name:          "test",
		Role:          k3d.ServerRole,
		Image:         "rancher/k3s:v0.9.0",
		RuntimeLabels: map[string]string{k3d.LabelRole: string(k3d.ServerRole)},
		Networks:      []string{"mynet"},
	}

	inputNode.FillRuntimeLabels()

	repr, err := TranslateNodeToContainer(inputNode)
	require.NoError(t, err)
	// Empty means "use the daemon default", so we must not set Runtime at all.
	assert.Empty(t, repr.HostConfig.Runtime, "unset DockerRuntime must leave HostConfig.Runtime empty")
	assert.NotContains(t, repr.ContainerConfig.Labels, k3d.LabelNodeDockerRuntime, "unset DockerRuntime must not create a runtime label")
}

func TestTranslateContainerDetailsToNodeDockerRuntime(t *testing.T) {
	// Build a minimal but valid k3d-managed container inspect result.
	// State.Running=false keeps us out of the IP-parsing branch, which
	// otherwise needs a real network address.
	labels := map[string]string{
		k3d.LabelRole:              string(k3d.ServerRole),
		k3d.LabelNodeDockerRuntime: "nvidia",
	}
	for k, v := range k3d.DefaultRuntimeLabels {
		labels[k] = v
	}

	details := dockertypes.ContainerJSON{
		ContainerJSONBase: &container.ContainerJSONBase{
			Name:  "/test",
			Image: "rancher/k3s:v0.9.0",
			State: &container.State{Running: false, Status: "exited"},
			HostConfig: &container.HostConfig{
				Runtime: "nvidia", // what the daemon reports for the created container
			},
		},
		Config:          &container.Config{Labels: labels},
		NetworkSettings: &container.NetworkSettings{},
	}

	node, err := TranslateContainerDetailsToNode(details)
	require.NoError(t, err)
	assert.Equal(t, "nvidia", node.DockerRuntime, "the runtime label must round-trip back to node.DockerRuntime")
}

func TestTranslateContainerDetailsToNodeDockerRuntimeDaemonDefault(t *testing.T) {
	// A daemon with default-runtime=nvidia reports the *resolved* runtime in
	// HostConfig.Runtime even though the user never requested one. Without
	// the k3d.node.dockerRuntime label, node reconstruction must NOT adopt
	// that value — otherwise every recreated node would pin the daemon
	// default explicitly.
	labels := map[string]string{k3d.LabelRole: string(k3d.ServerRole)}
	for k, v := range k3d.DefaultRuntimeLabels {
		labels[k] = v
	}

	details := dockertypes.ContainerJSON{
		ContainerJSONBase: &container.ContainerJSONBase{
			Name:  "/test",
			Image: "rancher/k3s:v0.9.0",
			State: &container.State{Running: false, Status: "exited"},
			HostConfig: &container.HostConfig{
				Runtime: "nvidia", // resolved daemon default, not a user request
			},
		},
		Config:          &container.Config{Labels: labels},
		NetworkSettings: &container.NetworkSettings{},
	}

	node, err := TranslateContainerDetailsToNode(details)
	require.NoError(t, err)
	assert.Empty(t, node.DockerRuntime, "a daemon-resolved runtime without the runtime label must not be adopted as an explicit DockerRuntime")
}
