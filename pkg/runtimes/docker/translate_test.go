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
