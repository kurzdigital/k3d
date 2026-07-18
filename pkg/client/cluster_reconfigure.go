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
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	l "github.com/k3d-io/k3d/v5/pkg/logger"
	k3drt "github.com/k3d-io/k3d/v5/pkg/runtimes"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

// replaceNodeTimeout bounds one full node replacement inside a rolling
// reconfigure (etcd rotation, container recreate incl. a potential image
// pull, start and readiness wait). Deliberately generous — aborting kicks
// off NodeReplace's rollback, so a timeout on a slow image pull is
// recoverable but annoying.
const replaceNodeTimeout = 15 * time.Minute

// ClusterReconfigureOpts configures a rolling reconfigure run.
type ClusterReconfigureOpts struct {
	// Image, if non-empty, replaces the k3s image of every server and agent
	// node (loadbalancer is left alone).
	Image string

	// Force allows reconfigure on a single-server cluster, accepting the API
	// downtime caused by replacing the only control plane node.
	Force bool

	// DrainTimeout caps how long `kubectl drain` may run per agent.
	DrainTimeout time.Duration

	// ReadyTimeout caps how long we wait for a replaced node to become Ready
	// before failing the reconfigure.
	ReadyTimeout time.Duration
}

// hasChanges returns true iff the changeset would actually mutate any node.
func (o ClusterReconfigureOpts) hasChanges() bool {
	return o.Image != ""
}

// ClusterReconfigure walks the cluster's nodes one at a time and replaces
// each container with one matching the requested changeset (currently:
// k3s image). Built on top of RollingApply, with a per-node op that drives
// NodeReplace via NodeEdit.
//
// The etcd init server is replaced last among servers, after its etcd
// membership has been removed from the cluster via the etcd gRPC API and
// its persistent etcd data wiped. The new container then joins the
// cluster as a regular peer (via K3S_URL/K3S_TOKEN) instead of trying to
// resume from stale state — that resume path crashes k3s with
// `panic: removed all voters` (k3s-io/k3s#8148).
func ClusterReconfigure(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, opts ClusterReconfigureOpts) error {
	if !opts.hasChanges() {
		return errors.New("nothing to reconfigure: no fields specified in changeset")
	}

	changeset := buildNodeChangesetFromOpts(opts)
	op := makeReplaceOp(changeset)

	if err := RollingApply(ctx, runtime, cluster, RollingApplyOpts{
		Verb:         "Reconfiguring",
		Force:        opts.Force,
		DrainTimeout: opts.DrainTimeout,
		ReadyTimeout: opts.ReadyTimeout,
		Op:           op,
	}); err != nil {
		// Not automatically resumable: aborting mid-traversal can leave a
		// mixed node set and (in HA) a half-rotated etcd membership. Make
		// that explicit at the point of failure instead of relying on the
		// operator remembering the --help warning.
		return fmt.Errorf("rolling reconfigure of cluster '%s' failed and is NOT automatically resumable — the cluster may be left with mixed node specs and, in HA, a half-rotated etcd membership (see `k3d cluster reconfigure --help` for recovery): %w", cluster.Name, err)
	}
	return nil
}

// buildNodeChangesetFromOpts translates the cluster-level reconfigure
// options into a per-node changeset. Kept tiny on purpose: extending this
// is the intended growth path (k3s args, env, labels).
func buildNodeChangesetFromOpts(opts ClusterReconfigureOpts) *NodeEditChangeset {
	cs := &NodeEditChangeset{}
	if opts.Image != "" {
		img := opts.Image
		cs.Image = &img
	}
	return cs
}

// makeReplaceOp returns a PerNodeOp that recreates the container via
// NodeEdit/NodeReplace, applying the given changeset.
//
// For server nodes in HA, a clean etcd member rotation runs first:
// remove the old peer's etcd membership via the etcd gRPC API, wipe its
// etcd data dir on the old container, then let the new container join
// the cluster as a regular peer (via K3S_URL/K3S_TOKEN). This avoids
// every form of "etcd resumes from stale state" — including the init
// server's force-new-cluster panic (k3s-io/k3s#8148) and any
// IP-change-vs-peer-URL mismatch on non-init servers — by treating
// every server replacement as a clean rotation.
//
// Single-server clusters (--force) skip the rotation: no peer to talk to,
// and the existing etcd data on the preserved volume is the cluster.
//
// Pre-flight always:
//   - Drop the k3s `<node>.node-password.k3s` secret and the
//     corresponding Node object. The new container generates a fresh
//     password file; without dropping the secret, the apiserver rejects
//     re-registration with "Node password rejected".
//
// Post-flight per server: refresh the loadbalancer config so its
// DNS-based upstreams pick up the new container IP.
func makeReplaceOp(changeset *NodeEditChangeset) PerNodeOp {
	return makeReplaceOpFn(func(*k3d.Node) *NodeEditChangeset { return changeset })
}

// makeReplaceOpFn is the per-node-changeset variant of makeReplaceOp: the
// changeset to apply is resolved per node, which lets `reconfigure -c`
// apply different spec changes to different nodes in one traversal.
//
// The whole per-node replacement is bounded by replaceNodeTimeout: without
// a bound, a replacement container whose k3s process dies before logging
// readiness (e.g. a k3s arg that crashes the kubelet) would hang the
// NodeStart wait — and thereby the whole reconfigure — forever.
func makeReplaceOpFn(changesetFor func(node *k3d.Node) *NodeEditChangeset) PerNodeOp {
	return func(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, node *k3d.Node) error {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, replaceNodeTimeout)
		defer cancel()

		changeset := changesetFor(node)
		if changeset == nil {
			return fmt.Errorf("no changeset resolved for node '%s' — filter and changeset map out of sync", node.Name)
		}
		// k3s stores a per-node password as <node>.node-password.k3s in
		// kube-system. NodeReplace destroys /etc/rancher/node/password
		// along with the old container; the new container generates a
		// fresh one and gets rejected by the apiserver because the secret
		// still holds the old hash. Drop both the secret and the Node
		// object so the new node can register cleanly.
		if execServer, err := pickExecServer(cluster, node); err == nil {
			cleanupCmd := []string{
				"sh", "-c",
				fmt.Sprintf(
					"kubectl -n kube-system delete secret %s.node-password.k3s --ignore-not-found; kubectl delete node %s --ignore-not-found",
					node.Name, node.Name,
				),
			}
			// 90s rather than 30s: under DIND-in-DIND load, after several
			// servers have been cycled, even short kubectl calls can take
			// the docker exec status round-trip longer than 30s. The
			// kubectl call itself is fast; the timeout was hitting the
			// exec-inspect call, not the work.
			cleanupCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			err := runtime.ExecInNode(cleanupCtx, execServer, cleanupCmd)
			cancel()
			if err != nil {
				return fmt.Errorf("failed to clear node-password secret / Node object for '%s' before replacement (the new container would otherwise fail to register): %w", node.Name, err)
			}
		} else {
			l.Log().Warnf("no exec server available to clear node-password for '%s' (%v) — replacement may fail to register", node.Name, err)
		}

		// Server-specific etcd member rotation, applied uniformly to
		// init and non-init servers in HA. See function doc.
		if node.Role == k3d.ServerRole {
			peer, peerErr := pickExecServer(cluster, node)
			isHA := peerErr == nil && peer.Name != node.Name
			if isHA {
				// Without a valid IP the member matching below would come up
				// empty and be misread as "already removed" — leaving a stale
				// member behind while the replacement joins as an extra peer.
				if !node.IP.IP.IsValid() {
					return fmt.Errorf("cannot rotate etcd member for server '%s': node IP unknown (container not running or not attached to the cluster network)", node.Name)
				}
				targetIP := node.IP.IP.String()
				if err := removeEtcdMember(ctx, runtime, peer, targetIP); err != nil {
					return fmt.Errorf("failed to remove server '%s' from etcd cluster: %w", node.Name, err)
				}
				wipeCtx, cancelWipe := context.WithTimeout(ctx, 30*time.Second)
				err := runtime.ExecInNode(wipeCtx, node, []string{"sh", "-c", "rm -rf /var/lib/rancher/k3s/server/db/etcd"})
				cancelWipe()
				if err != nil {
					return fmt.Errorf("failed to wipe etcd data on server '%s': %w", node.Name, err)
				}

				// Init server has no K3S_URL/K3S_TOKEN env from its
				// original bootstrap — inject them so the new container
				// joins the cluster instead of trying to resume the
				// (now-empty) bootstrap state. Non-init servers already
				// carry these in their preserved env.
				if node.ServerOpts.IsInit {
					token, err := readK3sNodeToken(ctx, runtime, peer)
					if err != nil {
						return fmt.Errorf("failed to read K3s node token from peer '%s': %w", peer.Name, err)
					}
					joinURL, err := pickJoinURL(cluster, node)
					if err != nil {
						return fmt.Errorf("failed to determine join URL for init-server '%s': %w", node.Name, err)
					}
					node.Env = append(node.Env, "K3S_URL="+joinURL, "K3S_TOKEN="+token)
					l.Log().Debugf("init-server '%s' will rejoin via %s after replacement", node.Name, joinURL)
				}
			}

			// Strip --cluster-init unconditionally on the init server.
			// On non-init servers it's a no-op (flag not present).
			//
			// Why also in the single-server / --force path (where isHA=false
			// and we did NOT inject K3S_URL/K3S_TOKEN above): the etcd data
			// dir is intact on the preserved volume, so the new container
			// resumes the existing cluster regardless of whether the flag
			// is present. K3s' bootstrap-from-empty path is what would
			// panic with `removed all voters` (k3s-io/k3s#8148), and we
			// don't trigger it because we don't wipe the data here.
			// Stripping is therefore harmless either way and keeps the
			// path uniform.
			if node.ServerOpts.IsInit {
				filtered := make([]string, 0, len(node.Cmd))
				for _, arg := range node.Cmd {
					if arg == "--cluster-init" {
						l.Log().Debugf("stripping --cluster-init from init server '%s' before replacement", node.Name)
						continue
					}
					filtered = append(filtered, arg)
				}
				node.Cmd = filtered
			}
		}

		if err := NodeEdit(ctx, runtime, node, changeset); err != nil {
			return fmt.Errorf("NodeEdit failed for '%s': %w", node.Name, err)
		}

		// `/run` is tmpfs, so the new container starts without
		// /run/flannel/subnet.env. Embedded Flannel takes a moment to
		// re-bootstrap after the k3s process is up. Kubelet marks the
		// node Ready before that — but pods scheduled on the new node in
		// the gap fail PodSandbox creation with
		// "loadFlannelSubnetEnv failed: open /run/flannel/subnet.env: no such file"
		// and only recover on retry. Waiting briefly here closes that
		// window. Bounded so we don't hang on non-Flannel setups.
		waitCNICtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_ = runtime.ExecInNode(waitCNICtx, node, []string{
			"sh", "-c",
			"i=0; while [ ! -f /run/flannel/subnet.env ] && [ $i -lt 30 ]; do sleep 1; i=$((i+1)); done",
		})
		cancel()

		// Refresh LB config after server replacements — DNS-based upstreams
		// resolve the new container IP automatically, but explicitly
		// re-rendering the config triggers the LB confd watcher to reload
		// nginx, avoiding a brief stale window.
		if node.Role == k3d.ServerRole {
			if reloaded, err := ClusterGet(ctx, runtime, cluster); err == nil {
				if errLB := UpdateLoadbalancerConfig(ctx, runtime, reloaded); errLB != nil {
					if !errors.Is(errLB, ErrLBConfigHostNotFound) {
						l.Log().Warnf("failed to refresh loadbalancer config after replacing server '%s': %v", node.Name, errLB)
					}
				}
			} else {
				l.Log().Warnf("failed to reload cluster after replacing server '%s' (loadbalancer config not refreshed): %v", node.Name, err)
			}
		}

		return nil
	}
}

const etcdCertDirInNode = "/var/lib/rancher/k3s/server/tls/etcd"

// readNodeFile reads a file from inside a node's container via the
// runtime's tar-based file copy (CopyFromContainer semantics: a tar
// stream wrapping the single requested file).
func readNodeFile(ctx context.Context, runtime k3drt.Runtime, node *k3d.Node, path string) ([]byte, error) {
	rd, err := runtime.ReadFromNode(ctx, path, node)
	if err != nil {
		return nil, fmt.Errorf("read %s on '%s': %w", path, node.Name, err)
	}
	defer rd.Close()
	body, err := extractSingleFileFromTar(rd)
	if err != nil {
		return nil, fmt.Errorf("extract %s from tar stream of '%s': %w", path, node.Name, err)
	}
	return body, nil
}

// extractSingleFileFromTar returns the content of the first regular file in
// a tar stream. Uses archive/tar instead of hand-stripping the 512-byte
// header so PAX/extended headers (which some daemons emit) are handled
// transparently. Pure so it can be unit-tested without a runtime.
func extractSingleFileFromTar(r io.Reader) ([]byte, error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("tar stream contained no regular file")
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
}

// etcdClient dials the given peer's etcd port over mTLS and returns a
// gRPC connection plus a Cluster service client. Reads the server-CA /
// client cert / client key from inside the peer container via the runtime
// — the k3s image has no HTTP-with-mTLS tools, so we connect from k3d's
// process over the Docker bridge network instead. K3s exposes etcd as gRPC
// on :2379 with the HTTP/JSON gateway disabled, hence the raw gRPC client
// (raw HTTP returns 415 "invalid gRPC request content-type
// 'application/json'").
//
// We only need two RPCs (MemberList, MemberRemove), so we talk to
// etcdserverpb.ClusterClient directly instead of pulling in the full
// go.etcd.io/etcd/client/v3 stack. The caller must Close the returned conn.
func etcdClient(ctx context.Context, runtime k3drt.Runtime, peer *k3d.Node) (*grpc.ClientConn, etcdserverpb.ClusterClient, error) {
	caPEM, err := readNodeFile(ctx, runtime, peer, etcdCertDirInNode+"/server-ca.crt")
	if err != nil {
		return nil, nil, err
	}
	certPEM, err := readNodeFile(ctx, runtime, peer, etcdCertDirInNode+"/client.crt")
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := readNodeFile(ctx, runtime, peer, etcdCertDirInNode+"/client.key")
	if err != nil {
		return nil, nil, err
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, nil, fmt.Errorf("failed to parse etcd server-ca.crt from '%s'", peer.Name)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load etcd client cert from '%s': %w", peer.Name, err)
	}

	tlsConfig := &tls.Config{
		RootCAs:      caPool,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	target := fmt.Sprintf("%s:2379", peer.IP.IP.String())
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial etcd gRPC on '%s' (%s): %w", peer.Name, target, err)
	}
	return conn, etcdserverpb.NewClusterClient(conn), nil
}

// matchEtcdMemberByIP finds the etcd member whose peer URL host is exactly
// targetIP. It returns the member's ID and whether a match was found.
//
// Exact host matching (parse the peer URL, compare the host without port)
// rather than substring matching is quorum-critical: with substring
// matching, target "10.0.0.1" also matches members at "10.0.0.10" and
// "10.0.0.11", so we could remove the wrong (healthy) member and break the
// cluster. Matching by IP rather than name is still required because k3s
// appends a random suffix to each member name on every process incarnation
// (`<container>-<rand>`), so the name isn't predictable from the container
// name.
func matchEtcdMemberByIP(members []*etcdserverpb.Member, targetIP string) (uint64, bool) {
	for _, m := range members {
		for _, peerURL := range m.PeerURLs {
			host := peerURLHost(peerURL)
			if host != "" && host == targetIP {
				return m.ID, true
			}
		}
	}
	return 0, false
}

// peerURLHost extracts the host (without port) from an etcd peer URL such
// as "https://10.0.0.5:2380". Falls back to treating the raw value as a
// host:port pair (or bare host) if it doesn't parse as a URL.
func peerURLHost(peerURL string) string {
	if u, err := url.Parse(peerURL); err == nil && u.Host != "" {
		return stripPort(u.Host)
	}
	return stripPort(peerURL)
}

// stripPort removes a trailing :port from a host, leaving IPv4/hostname
// untouched. IPv6 literals (bracketed) keep their inner address.
func stripPort(host string) string {
	if h, ok := strings.CutPrefix(host, "["); ok {
		if i := strings.Index(h, "]"); i >= 0 {
			return h[:i]
		}
		return host
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return host[:i]
	}
	return host
}

// quorumPreservedAfterRemoval verifies that removing the member with
// removeID leaves a healthy voting majority. It counts the started voting
// members (non-learner, with a non-empty Name — etcd reports an empty Name
// for members that have not yet joined/started) that would remain, and
// requires them to be a strict majority of the post-removal voter set.
//
// This guards against firing MemberRemove into an already-degraded cluster:
// removing a voter from a cluster that has lost too many peers can drop it
// below quorum and wedge it. Returns a descriptive error if the invariant
// would be violated.
func quorumPreservedAfterRemoval(members []*etcdserverpb.Member, removeID uint64) error {
	totalVoters := 0
	startedRemainingVoters := 0
	for _, m := range members {
		if m.IsLearner {
			continue
		}
		totalVoters++
		if m.ID == removeID {
			continue
		}
		if m.Name != "" {
			startedRemainingVoters++
		}
	}

	// After removal, etcd's voter count drops by one (assuming removeID is a
	// voter, which the caller guarantees). Quorum of the remaining set is
	// floor(remainingVoters/2)+1.
	remainingVoters := totalVoters - 1
	if remainingVoters < 1 {
		return fmt.Errorf("refusing to remove etcd member: it is the only voting member (removal would destroy the cluster)")
	}
	needed := remainingVoters/2 + 1
	if startedRemainingVoters < needed {
		return fmt.Errorf("refusing to remove etcd member: only %d of %d remaining voting members are started, need at least %d for quorum (cluster is already degraded — recover the missing member(s) first)", startedRemainingVoters, remainingVoters, needed)
	}
	return nil
}

// removeEtcdMember finds the etcd member whose peer URL host is exactly
// targetIP and removes it from the cluster via etcd's gRPC API, after
// verifying the remaining members preserve quorum. Idempotent: a missing
// member is treated as already-removed.
func removeEtcdMember(ctx context.Context, runtime k3drt.Runtime, peer *k3d.Node, targetIP string) error {
	conn, cli, err := etcdClient(ctx, runtime, peer)
	if err != nil {
		return fmt.Errorf("build etcd client: %w", err)
	}
	defer conn.Close()

	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := cli.MemberList(listCtx, &etcdserverpb.MemberListRequest{})
	if err != nil {
		return fmt.Errorf("etcd member-list via %s (note: etcd on port 2379 must be reachable from the machine running k3d — container IPs are not routable from the client on Docker Desktop or with a remote DOCKER_HOST): %w", peer.Name, err)
	}

	memberID, found := matchEtcdMemberByIP(resp.Members, targetIP)
	if !found {
		l.Log().Debugf("etcd member with peer URL host %s not found — already removed?", targetIP)
		return nil
	}
	l.Log().Debugf("etcd member (id=%x) matched target IP %s", memberID, targetIP)

	if err := quorumPreservedAfterRemoval(resp.Members, memberID); err != nil {
		return fmt.Errorf("etcd quorum check via %s: %w", peer.Name, err)
	}

	rmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := cli.MemberRemove(rmCtx, &etcdserverpb.MemberRemoveRequest{ID: memberID}); err != nil {
		return fmt.Errorf("etcd member-remove (id=%x) via %s: %w", memberID, peer.Name, err)
	}
	l.Log().Infof("Removed etcd member id=%x (peer URL host %s) via %s", memberID, targetIP, peer.Name)
	return nil
}

// readK3sNodeToken reads the cluster's server node-token from a healthy
// peer. The token is what new servers must present in K3S_TOKEN to join
// the existing cluster as a peer.
func readK3sNodeToken(ctx context.Context, runtime k3drt.Runtime, peer *k3d.Node) (string, error) {
	body, err := readNodeFile(ctx, runtime, peer, "/var/lib/rancher/k3s/server/token")
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(body))
	if tok == "" {
		return "", fmt.Errorf("empty K3s node token read from '%s'", peer.Name)
	}
	return tok, nil
}

// pickJoinURL returns a stable peer URL the replaced init server can use to
// rejoin the cluster. Prefers the cluster's loadbalancer (DNS-resolvable
// inside the cluster network, survives single-peer churn), falls back to a
// concrete healthy peer's IP.
func pickJoinURL(cluster *k3d.Cluster, skip *k3d.Node) (string, error) {
	for _, n := range cluster.Nodes {
		if n.Role == k3d.LoadBalancerRole && n.State.Running {
			return fmt.Sprintf("https://%s:%s", n.Name, k3d.DefaultAPIPort), nil
		}
	}
	peer, err := pickExecServer(cluster, skip)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://%s:%s", peer.IP.IP.String(), k3d.DefaultAPIPort), nil
}
