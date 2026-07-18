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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

func TestKubectlDrainTimedOut(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil error":                {nil, false},
		"global timeout reached":   {errors.New("exit code '1': ... error: global timeout reached: 1m0s"), true},
		"per-pod context deadline": {errors.New(`There are pending pods ... error when waiting for pod "helm-install-traefik-x" in namespace "kube-system" to terminate: context deadline exceeded`), true},
		"pending pods":             {errors.New(`There are pending pods in node "k3d-x-agent-0" when an error occurred`), true},
		"apiserver unreachable":    {errors.New("Unable to connect to the server: dial tcp: connect: connection refused"), false},
		"rbac forbidden":           {errors.New("error: pods is forbidden: User cannot list resource"), false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.want, kubectlDrainTimedOut(c.err))
		})
	}
}

func server(name string, init, running bool) *k3d.Node {
	n := &k3d.Node{Name: name, Role: k3d.ServerRole}
	n.ServerOpts.IsInit = init
	n.State.Running = running
	return n
}

func agent(name string, running bool) *k3d.Node {
	n := &k3d.Node{Name: name, Role: k3d.AgentRole}
	n.State.Running = running
	return n
}

func names(nodes []*k3d.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

func TestPartitionNodesForRolling(t *testing.T) {
	tests := []struct {
		name        string
		nodes       []*k3d.Node
		wantInit    string // "" => nil
		wantServers []string
		wantAgents  []string
	}{
		{
			name:        "empty",
			nodes:       nil,
			wantServers: []string{},
			wantAgents:  []string{},
		},
		{
			name: "single init server",
			nodes: []*k3d.Node{
				server("s0", true, true),
			},
			wantInit:    "s0",
			wantServers: []string{},
			wantAgents:  []string{},
		},
		{
			name: "ha cluster with agents and infra dropped",
			nodes: []*k3d.Node{
				server("s0", true, true),
				server("s1", false, true),
				server("s2", false, true),
				agent("a0", true),
				agent("a1", true),
				{Name: "lb", Role: k3d.LoadBalancerRole},
				{Name: "reg", Role: k3d.RegistryRole},
			},
			wantInit:    "s0",
			wantServers: []string{"s1", "s2"},
			wantAgents:  []string{"a0", "a1"},
		},
		{
			name: "no init server",
			nodes: []*k3d.Node{
				server("s1", false, true),
				agent("a0", true),
			},
			wantInit:    "",
			wantServers: []string{"s1"},
			wantAgents:  []string{"a0"},
		},
		{
			name: "order preserved within groups",
			nodes: []*k3d.Node{
				server("s2", false, true),
				server("s1", false, true),
				server("s0", true, true),
			},
			wantInit:    "s0",
			wantServers: []string{"s2", "s1"},
			wantAgents:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initServer, servers, agents := partitionNodesForRolling(tt.nodes)
			if tt.wantInit == "" {
				assert.Nil(t, initServer)
			} else {
				require.NotNil(t, initServer)
				assert.Equal(t, tt.wantInit, initServer.Name)
			}
			assert.Equal(t, tt.wantServers, names(servers))
			assert.Equal(t, tt.wantAgents, names(agents))
		})
	}
}

func TestPickExecServer(t *testing.T) {
	tests := []struct {
		name    string
		nodes   []*k3d.Node
		skip    string // "" => nil
		want    string
		wantErr bool
	}{
		{
			name: "prefers another running server over the skipped one",
			nodes: []*k3d.Node{
				server("s0", true, true),
				server("s1", false, true),
			},
			skip: "s0",
			want: "s1",
		},
		{
			name: "falls back to skipped server when it is the only one running",
			nodes: []*k3d.Node{
				server("s0", true, true),
				server("s1", false, false), // stopped
			},
			skip: "s0",
			want: "s0",
		},
		{
			name: "skips stopped servers",
			nodes: []*k3d.Node{
				server("s0", false, false),
				server("s1", false, true),
			},
			want: "s1",
		},
		{
			name: "ignores agents",
			nodes: []*k3d.Node{
				agent("a0", true),
				server("s0", true, true),
			},
			want: "s0",
		},
		{
			name: "no running server",
			nodes: []*k3d.Node{
				server("s0", true, false),
				agent("a0", true),
			},
			wantErr: true,
		},
		{
			name:    "no servers at all",
			nodes:   []*k3d.Node{agent("a0", true)},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := &k3d.Cluster{Name: "test", Nodes: tt.nodes}
			var skip *k3d.Node
			if tt.skip != "" {
				skip = &k3d.Node{Name: tt.skip}
			}
			got, err := pickExecServer(cluster, skip)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tt.want, got.Name)
		})
	}
}

func TestCheckRollingPreconditions(t *testing.T) {
	withDatastore := &k3d.Cluster{Name: "ds", ExternalDatastore: &k3d.ExternalDatastore{}}
	noDatastore := &k3d.Cluster{Name: "nods"}

	tests := []struct {
		name             string
		cluster          *k3d.Cluster
		serverTotal      int
		serversRunning   int
		entrypointActive bool
		agentOnly        bool // traversal touches no server nodes
		force            bool
		wantErr          bool
	}{
		{
			name:             "ok: 3 servers, entrypoint active",
			cluster:          noDatastore,
			serverTotal:      3,
			serversRunning:   3,
			entrypointActive: true,
		},
		{
			name:             "abort: not all servers running",
			cluster:          noDatastore,
			serverTotal:      3,
			serversRunning:   2,
			entrypointActive: true,
			wantErr:          true,
		},
		{
			name:             "abort: not all servers running even with force",
			cluster:          noDatastore,
			serverTotal:      3,
			serversRunning:   2,
			entrypointActive: true,
			force:            true,
			wantErr:          true,
		},
		{
			name:             "abort: single server, no datastore, no force",
			cluster:          noDatastore,
			serverTotal:      1,
			serversRunning:   1,
			entrypointActive: true,
			wantErr:          true,
		},
		{
			name:             "ok: single server, no datastore, with force",
			cluster:          noDatastore,
			serverTotal:      1,
			serversRunning:   1,
			entrypointActive: true,
			force:            true,
		},
		{
			name:             "ok: single server with external datastore",
			cluster:          withDatastore,
			serverTotal:      1,
			serversRunning:   1,
			entrypointActive: true,
		},
		{
			name:             "abort: servers without active entrypoint, no force",
			cluster:          noDatastore,
			serverTotal:      3,
			serversRunning:   3,
			entrypointActive: false,
			wantErr:          true,
		},
		{
			name:             "ok: servers without active entrypoint, with force",
			cluster:          noDatastore,
			serverTotal:      3,
			serversRunning:   3,
			entrypointActive: false,
			force:            true,
		},
		{
			name:             "ok: no servers at all, entrypoint check skipped",
			cluster:          noDatastore,
			serverTotal:      0,
			serversRunning:   0,
			entrypointActive: false,
			force:            true, // force needed for the <2-server datastore guard
		},
		{
			name:             "ok: agent-only traversal waives server preconditions",
			cluster:          noDatastore,
			serverTotal:      1,
			serversRunning:   1,
			entrypointActive: false,
			agentOnly:        true,
		},
		{
			name:             "abort: agent-only traversal still requires all servers running",
			cluster:          noDatastore,
			serverTotal:      2,
			serversRunning:   1,
			entrypointActive: true,
			agentOnly:        true,
			wantErr:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkRollingPreconditions(tt.cluster, tt.serverTotal, tt.serversRunning, tt.entrypointActive, !tt.agentOnly, tt.force)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestClusterHasExternalDatastore(t *testing.T) {
	tests := []struct {
		name    string
		cluster *k3d.Cluster
		want    bool
	}{
		{
			name: "detected via env",
			cluster: &k3d.Cluster{Nodes: []*k3d.Node{
				{Role: k3d.ServerRole, Env: []string{"K3S_TOKEN=abc", "K3S_DATASTORE_ENDPOINT=mysql://db"}},
			}},
			want: true,
		},
		{
			name: "detected via arg",
			cluster: &k3d.Cluster{Nodes: []*k3d.Node{
				{Role: k3d.ServerRole, Cmd: []string{"server", "--datastore-endpoint=mysql://db"}},
			}},
			want: true,
		},
		{
			name: "detected via split arg",
			cluster: &k3d.Cluster{Nodes: []*k3d.Node{
				{Role: k3d.ServerRole, Cmd: []string{"server"}, Args: []string{"--datastore-endpoint", "mysql://db"}},
			}},
			want: true,
		},
		{
			name: "embedded etcd cluster",
			cluster: &k3d.Cluster{Nodes: []*k3d.Node{
				{Role: k3d.ServerRole, Cmd: []string{"server", "--cluster-init"}, Env: []string{"K3S_TOKEN=abc"}},
				{Role: k3d.AgentRole, Env: []string{"K3S_DATASTORE_ENDPOINT=red-herring"}}, // agents don't count
			}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, clusterHasExternalDatastore(tt.cluster))
		})
	}
}

func TestServerEntrypointActive(t *testing.T) {
	tests := []struct {
		name    string
		cluster *k3d.Cluster
		want    bool
	}{
		{
			name: "all servers with entrypoint",
			cluster: &k3d.Cluster{Nodes: []*k3d.Node{
				{Role: k3d.ServerRole, K3dEntrypoint: true},
				{Role: k3d.ServerRole, K3dEntrypoint: true},
				{Role: k3d.AgentRole, K3dEntrypoint: false}, // agents don't count
			}},
			want: true,
		},
		{
			name: "one server without entrypoint",
			cluster: &k3d.Cluster{Nodes: []*k3d.Node{
				{Role: k3d.ServerRole, K3dEntrypoint: true},
				{Role: k3d.ServerRole, K3dEntrypoint: false},
			}},
			want: false,
		},
		{
			name:    "no servers at all",
			cluster: &k3d.Cluster{Nodes: []*k3d.Node{{Role: k3d.LoadBalancerRole}}},
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, serverEntrypointActive(tt.cluster))
		})
	}
}
