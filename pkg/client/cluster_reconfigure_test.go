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
	"archive/tar"
	"bytes"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"

	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

func member(id uint64, name string, learner bool, peerURLs ...string) *etcdserverpb.Member {
	return &etcdserverpb.Member{ID: id, Name: name, IsLearner: learner, PeerURLs: peerURLs}
}

func TestMatchEtcdMemberByIP(t *testing.T) {
	members := []*etcdserverpb.Member{
		member(1, "s0", false, "https://10.0.0.1:2380"),
		member(10, "s1", false, "https://10.0.0.10:2380"),
		member(11, "s2", false, "https://10.0.0.11:2380"),
	}

	tests := []struct {
		name     string
		members  []*etcdserverpb.Member
		targetIP string
		wantID   uint64
		wantOK   bool
	}{
		{
			// The whole point: substring matching would also match
			// 10.0.0.10 and 10.0.0.11, removing the wrong member.
			name:     "exact match does not greedily match .10/.11 for .1",
			members:  members,
			targetIP: "10.0.0.1",
			wantID:   1,
			wantOK:   true,
		},
		{
			name:     "matches .10 exactly",
			members:  members,
			targetIP: "10.0.0.10",
			wantID:   10,
			wantOK:   true,
		},
		{
			name:     "matches .11 exactly",
			members:  members,
			targetIP: "10.0.0.11",
			wantID:   11,
			wantOK:   true,
		},
		{
			name:     "no match returns not found",
			members:  members,
			targetIP: "10.0.0.99",
			wantOK:   false,
		},
		{
			name: "matches across multiple peer URLs on one member",
			members: []*etcdserverpb.Member{
				member(7, "s7", false, "https://10.0.0.7:2380", "https://172.16.0.7:2380"),
			},
			targetIP: "172.16.0.7",
			wantID:   7,
			wantOK:   true,
		},
		{
			name: "ignores port differences (host-only compare)",
			members: []*etcdserverpb.Member{
				member(5, "s5", false, "https://10.0.0.5:9999"),
			},
			targetIP: "10.0.0.5",
			wantID:   5,
			wantOK:   true,
		},
		{
			name:     "empty members",
			members:  nil,
			targetIP: "10.0.0.1",
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := matchEtcdMemberByIP(tt.members, tt.targetIP)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantID, id)
			}
		})
	}
}

func TestPeerURLHost(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://10.0.0.5:2380", "10.0.0.5"},
		{"http://10.0.0.5:2380", "10.0.0.5"},
		{"https://node-name:2380", "node-name"},
		{"10.0.0.5:2380", "10.0.0.5"},
		{"10.0.0.5", "10.0.0.5"},
		{"https://[fd00::1]:2380", "fd00::1"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, peerURLHost(tt.in))
		})
	}
}

func TestQuorumPreservedAfterRemoval(t *testing.T) {
	tests := []struct {
		name     string
		members  []*etcdserverpb.Member
		removeID uint64
		wantErr  bool
	}{
		{
			// 3 voters, all started -> remove one leaves 2 started, quorum=2. OK.
			name: "3-node healthy cluster, remove one",
			members: []*etcdserverpb.Member{
				member(1, "s0", false, "https://10.0.0.1:2380"),
				member(2, "s1", false, "https://10.0.0.2:2380"),
				member(3, "s2", false, "https://10.0.0.3:2380"),
			},
			removeID: 1,
			wantErr:  false,
		},
		{
			// 3 voters but one of the *remaining* two is not started
			// (Name=="") -> only 1 started remains, quorum=2. Refuse.
			name: "3-node with one remaining peer not started",
			members: []*etcdserverpb.Member{
				member(1, "s0", false, "https://10.0.0.1:2380"),
				member(2, "s1", false, "https://10.0.0.2:2380"),
				member(3, "", false, "https://10.0.0.3:2380"),
			},
			removeID: 1,
			wantErr:  true,
		},
		{
			// Single-voter cluster: removal destroys it.
			name: "single voter",
			members: []*etcdserverpb.Member{
				member(1, "s0", false, "https://10.0.0.1:2380"),
			},
			removeID: 1,
			wantErr:  true,
		},
		{
			// 2 voters, both started -> remove one leaves 1 started,
			// quorum of remaining (1) is 1. OK.
			name: "2-node both started, remove one",
			members: []*etcdserverpb.Member{
				member(1, "s0", false, "https://10.0.0.1:2380"),
				member(2, "s1", false, "https://10.0.0.2:2380"),
			},
			removeID: 1,
			wantErr:  false,
		},
		{
			// 2 voters, the survivor is not started -> 0 started remain,
			// quorum=1. Refuse.
			name: "2-node survivor not started",
			members: []*etcdserverpb.Member{
				member(1, "s0", false, "https://10.0.0.1:2380"),
				member(2, "", false, "https://10.0.0.2:2380"),
			},
			removeID: 1,
			wantErr:  true,
		},
		{
			// Learners do not count toward voter quorum: a started learner
			// among the remaining set does not rescue quorum.
			name: "learner does not count toward quorum",
			members: []*etcdserverpb.Member{
				member(1, "s0", false, "https://10.0.0.1:2380"),
				member(2, "s1", false, "https://10.0.0.2:2380"),
				member(3, "l0", true, "https://10.0.0.3:2380"),
			},
			removeID: 1,
			wantErr:  false, // 2 voters, remove 1 -> 1 started voter, quorum=1
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := quorumPreservedAfterRemoval(tt.members, tt.removeID)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestPickJoinURL(t *testing.T) {
	lbNode := func(name string, running bool) *k3d.Node {
		n := &k3d.Node{Name: name, Role: k3d.LoadBalancerRole}
		n.State.Running = running
		return n
	}
	serverWithIP := func(name, ip string, running bool) *k3d.Node {
		n := &k3d.Node{Name: name, Role: k3d.ServerRole}
		n.State.Running = running
		n.IP.IP = netip.MustParseAddr(ip)
		return n
	}

	t.Run("prefers running loadbalancer by name", func(t *testing.T) {
		cluster := &k3d.Cluster{Name: "c", Nodes: []*k3d.Node{
			lbNode("c-lb", true),
			serverWithIP("c-s0", "10.0.0.1", true),
			serverWithIP("c-s1", "10.0.0.2", true),
		}}
		skip := cluster.Nodes[1]
		got, err := pickJoinURL(cluster, skip)
		require.NoError(t, err)
		assert.Equal(t, "https://c-lb:"+k3d.DefaultAPIPort, got)
	})

	t.Run("falls back to a peer server IP when no running LB", func(t *testing.T) {
		cluster := &k3d.Cluster{Name: "c", Nodes: []*k3d.Node{
			lbNode("c-lb", false), // stopped LB is skipped
			serverWithIP("c-s0", "10.0.0.1", true),
			serverWithIP("c-s1", "10.0.0.2", true),
		}}
		skip := cluster.Nodes[1] // skip c-s0, expect c-s1's IP
		got, err := pickJoinURL(cluster, skip)
		require.NoError(t, err)
		assert.Equal(t, "https://10.0.0.2:"+k3d.DefaultAPIPort, got)
	})

	t.Run("errors when no running server and no LB", func(t *testing.T) {
		cluster := &k3d.Cluster{Name: "c", Nodes: []*k3d.Node{
			serverWithIP("c-s0", "10.0.0.1", false),
		}}
		_, err := pickJoinURL(cluster, cluster.Nodes[0])
		require.Error(t, err)
	})
}

func TestExtractSingleFileFromTar(t *testing.T) {
	// readNodeFile unwraps the tar stream that runtime.ReadFromNode
	// (CopyFromContainer semantics) delivers. extractSingleFileFromTar is
	// that pure unwrapping logic.
	mkTar := func(entries ...struct {
		name     string
		typeflag byte
		content  []byte
	}) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, e := range entries {
			hdr := &tar.Header{
				Name:     e.name,
				Typeflag: e.typeflag,
				Mode:     0o600,
				Size:     int64(len(e.content)),
			}
			require.NoError(t, tw.WriteHeader(hdr))
			if e.typeflag == tar.TypeReg {
				_, err := tw.Write(e.content)
				require.NoError(t, err)
			}
		}
		require.NoError(t, tw.Close())
		return buf.Bytes()
	}
	type entry = struct {
		name     string
		typeflag byte
		content  []byte
	}

	t.Run("single regular file", func(t *testing.T) {
		got, err := extractSingleFileFromTar(bytes.NewReader(mkTar(entry{"token", tar.TypeReg, []byte("hello-token\n")})))
		require.NoError(t, err)
		assert.Equal(t, []byte("hello-token\n"), got)
	})

	t.Run("directory entry before regular file is skipped", func(t *testing.T) {
		got, err := extractSingleFileFromTar(bytes.NewReader(mkTar(
			entry{"etcd/", tar.TypeDir, nil},
			entry{"etcd/server-ca.crt", tar.TypeReg, []byte("PEM")},
		)))
		require.NoError(t, err)
		assert.Equal(t, []byte("PEM"), got)
	})

	t.Run("empty regular file yields empty content", func(t *testing.T) {
		got, err := extractSingleFileFromTar(bytes.NewReader(mkTar(entry{"empty", tar.TypeReg, nil})))
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("stream without regular file errors", func(t *testing.T) {
		_, err := extractSingleFileFromTar(bytes.NewReader(mkTar(entry{"dir/", tar.TypeDir, nil})))
		require.Error(t, err)
	})

	t.Run("garbage input errors", func(t *testing.T) {
		_, err := extractSingleFileFromTar(bytes.NewReader([]byte("this is not a tar stream")))
		require.Error(t, err)
	})
}
