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
	"fmt"
	"sort"
	"strings"

	"github.com/docker/go-connections/nat"
	dockerunits "github.com/docker/go-units"

	"github.com/k3d-io/k3d/v5/pkg/actions"
	conf "github.com/k3d-io/k3d/v5/pkg/config/v1alpha5"
	l "github.com/k3d-io/k3d/v5/pkg/logger"
	k3drt "github.com/k3d-io/k3d/v5/pkg/runtimes"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
	"github.com/k3d-io/k3d/v5/pkg/types/k3s"
	"github.com/k3d-io/k3d/v5/pkg/util"
)

/*
 * `cluster reconfigure -c <simpleconfig>`: diff a desired SimpleConfig
 * against the actual (reconstructed) cluster state and roll the differing
 * nodes through the existing rolling-replace machinery.
 *
 * The desired per-node specs come from the same translation path `cluster
 * create` uses (TransformSimpleToClusterConfig) — there is deliberately no
 * second config-to-node mapping. The actual state comes from the cluster
 * reconstruction k3d already does (container inspect / labels).
 *
 * Because node creation injects k3d-managed items into the very same fields
 * users configure (tls-san args, --node-label args, K3S_TOKEN/K3S_URL env,
 * image volume and meminfo mounts, ...), both sides are first projected onto
 * their canonical *user-scope* form before comparing. The same projection
 * drives the apply step, so applying a config and re-diffing it yields
 * all-no-op (idempotency), and removing an entry from the config removes it
 * from the node on the next apply (revocation).
 */

// NodeSpecChangeset is the desired user-scope spec for a single node,
// applied by NodeEdit as a full replacement of the user-scope portion of
// the node while preserving the k3d-managed portion.
type NodeSpecChangeset struct {
	Image         string
	Volumes       []string // volume specs (src:dest[:opts])
	Args          []string // k3s args
	Env           []string // KEY=VALUE
	K3sNodeLabels map[string]string
	RuntimeLabels map[string]string // merged on top, never a replace trigger (not reconstructable)
	Memory        string
	Files         []k3d.File // (re)written on replace, never a replace trigger (drift not detected)
}

// FieldDiff describes one differing user-scope field on a node.
type FieldDiff struct {
	Field   string
	Current []string
	Desired []string
}

// NodeSpecDiff collects the differing fields of one node plus the changeset
// that would align it with the desired spec.
type NodeSpecDiff struct {
	NodeName  string
	Role      k3d.Role
	Changes   []FieldDiff
	Changeset *NodeSpecChangeset
}

// LoadBalancerSpecDiff describes serverlb host-port-mapping differences,
// including a change of the explicitly requested kube-api host port. It is
// applied by replacing only the loadbalancer container — no node drain, no
// etcd rotation (host port bindings live exclusively on the serverlb).
type LoadBalancerSpecDiff struct {
	NodeName string
	// Added / Removed are the host port mappings only present in the config
	// / only present on the running loadbalancer, rendered as
	// "port->hostip:hostport" strings (kube-api mapping excluded).
	Added   []string
	Removed []string
	// KubeAPI is the kube-api host port change, if any. Only an explicitly
	// requested, non-"random" port participates (the host side is
	// auto-assigned at create time otherwise); the HostIP is not diffed.
	KubeAPI *FieldDiff
	// DesiredPorts is the complete desired port set for the replacement
	// container: the config's port mappings plus the kube-api binding
	// (resolved against the actual one, see diffLoadBalancerSpec).
	DesiredPorts nat.PortMap
}

// ClusterSpecDiff is the result of diffing a desired cluster config against
// the actual cluster state.
type ClusterSpecDiff struct {
	// ChangedNodes lists nodes that need a rolling replace, in no
	// particular order (RollingApply establishes the traversal order).
	ChangedNodes []NodeSpecDiff
	// UnchangedNodes lists node names that already match the desired spec.
	UnchangedNodes []string
	// LoadBalancer, if non-nil, describes serverlb port-mapping changes to
	// be applied by replacing the loadbalancer container.
	LoadBalancer *LoadBalancerSpecDiff
	// Unsupported lists definite differences that cannot be applied by
	// replacing nodes (they require a full cluster recreate). Any entry
	// here aborts a non-dry-run apply.
	Unsupported []string
	// NotDiffable lists config aspects that were requested but cannot be
	// compared against the actual state; they are reported instead of
	// silently ignored.
	NotDiffable []string
}

// injectedEnvKeys are env vars that k3d (or the k3s image) sets on node
// containers; they are never part of the user-scope env comparison.
// K3S_TOKEN/K3S_URL are preserved verbatim on replace, PATH/CRI_CONFIG_FILE
// come from the image, K3S_KUBECONFIG_OUTPUT is re-added by NodeCreate.
var injectedEnvKeys = map[string]struct{}{
	"PATH":                   {},
	"CRI_CONFIG_FILE":        {},
	k3s.EnvClusterToken:      {},
	k3s.EnvClusterConnectURL: {},
	k3s.EnvKubeconfigOutput:  {},
}

// preservedEnvKeys are injected env vars that must survive a node replace
// verbatim (they carry cluster-join state that is not re-derivable at
// replace time).
var preservedEnvKeys = map[string]struct{}{
	k3s.EnvClusterToken:      {},
	k3s.EnvClusterConnectURL: {},
}

// userScopeEnv projects an env list onto its user-scope form: injected
// entries dropped, K3D_*-prefixed entries dropped.
func userScopeEnv(env []string) []string {
	result := []string{}
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		if _, injected := injectedEnvKeys[key]; injected {
			continue
		}
		if strings.HasPrefix(key, "K3D_") {
			continue
		}
		result = append(result, e)
	}
	return result
}

// preservedEnv returns the entries of env that must be carried over to a
// replacement container verbatim.
func preservedEnv(env []string) []string {
	result := []string{}
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		if _, keep := preservedEnvKeys[key]; keep {
			result = append(result, e)
		}
	}
	return result
}

// volumeSpecDest returns the destination path of a volume spec
// (src:dest[:opts] or dest-only for anonymous mounts).
func volumeSpecDest(spec string) string {
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) >= 2 {
		return parts[1]
	}
	return parts[0]
}

// managedVolume reports whether a volume spec on a reconstructed node is
// k3d-managed (image volume, faked meminfo/edac mounts for memory limits,
// volumes adopted from Docker-anonymous ones) rather than user-configured.
func managedVolume(spec string) bool {
	src, _, hasDest := strings.Cut(spec, ":")
	if hasDest && isAnonymousVolumeName(src) {
		return true
	}
	switch volumeSpecDest(spec) {
	case k3d.DefaultImageVolumeMountPath, util.MemInfoPath, util.EdacFolderPath:
		return true
	}
	return false
}

// userScopeVolumes projects a reconstructed node's volume list onto its
// user-scope form.
func userScopeVolumes(volumes []string) []string {
	result := []string{}
	for _, v := range volumes {
		if managedVolume(v) {
			continue
		}
		result = append(result, v)
	}
	return result
}

// managedVolumes is the complement of userScopeVolumes, minus the
// meminfo/edac fakes (NodeCreate regenerates those from Node.Memory, so
// carrying them over would duplicate the binds).
func managedVolumes(volumes []string) []string {
	result := []string{}
	for _, v := range volumes {
		if !managedVolume(v) {
			continue
		}
		switch volumeSpecDest(v) {
		case util.MemInfoPath, util.EdacFolderPath:
			continue
		}
		result = append(result, v)
	}
	return result
}

// userScopeArgs projects a node's full k3s invocation (Cmd+Args as
// reconstructed from the container) onto the user-scope args plus the k3s
// node labels encoded as `--node-label` pairs. Injected items — the role
// token, `--cluster-init`, and the `--tls-san` pairs k3d derives from the
// node's labels — are dropped.
func userScopeArgs(node *k3d.Node) (args []string, nodeLabels map[string]string) {
	full := append(append([]string{}, node.Cmd...), node.Args...)
	args = []string{}
	nodeLabels = map[string]string{}

	injectedSANs := map[string]struct{}{}
	if v := node.RuntimeLabels[k3d.LabelServerAPIHost]; v != "" {
		injectedSANs[v] = struct{}{}
	}
	if v := node.RuntimeLabels[k3d.LabelServerLoadBalancer]; v != "" {
		injectedSANs[v] = struct{}{}
	}

	for i := 0; i < len(full); i++ {
		arg := full[i]
		if i == 0 && (arg == "server" || arg == "agent") {
			continue
		}
		if arg == "--cluster-init" {
			continue
		}
		if arg == "--node-label" && i+1 < len(full) {
			k, v := util.SplitLabelKeyValue(full[i+1])
			nodeLabels[k] = v
			i++
			continue
		}
		if arg == "--tls-san" && i+1 < len(full) {
			if _, injected := injectedSANs[full[i+1]]; injected {
				i++
				continue
			}
		}
		args = append(args, arg)
	}
	return args, nodeLabels
}

// normalizeImageRef canonicalizes an image reference for comparison:
// the implicit docker.io/ registry and library/ namespace are stripped.
func normalizeImageRef(ref string) string {
	ref = strings.TrimPrefix(ref, "docker.io/")
	ref = strings.TrimPrefix(ref, "library/")
	return ref
}

// normalizeMemory parses a human memory limit ("1g", "512MiB", "") into a
// canonical byte-count string; empty input means "no limit" and stays "".
// Unparseable values are returned verbatim (they will simply compare
// unequal and surface in the diff).
func normalizeMemory(mem string) string {
	if mem == "" {
		return ""
	}
	bytes, err := dockerunits.RAMInBytes(mem)
	if err != nil {
		return mem
	}
	return fmt.Sprintf("%d", bytes)
}

// sortedSet returns a sorted copy of in with exact duplicates removed —
// the canonical order-insensitive form used for field comparison.
func sortedSet(in []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// labelsToSortedList renders a label map as a sorted k=v list.
func labelsToSortedList(labels map[string]string) []string {
	out := make([]string, 0, len(labels))
	for k, v := range labels {
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(out)
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// diffNodeSpec computes the user-scope field diff between the desired
// (transform-produced) and the actual (reconstructed) node.
func diffNodeSpec(target, actual *k3d.Node) NodeSpecDiff {
	diff := NodeSpecDiff{NodeName: actual.Name, Role: actual.Role}

	compare := func(field string, current, desired []string) {
		if !stringSlicesEqual(current, desired) {
			diff.Changes = append(diff.Changes, FieldDiff{Field: field, Current: current, Desired: desired})
		}
	}

	// An empty target image means "leave the image as it is" — a config
	// without an image field must not implicitly roll the cluster onto
	// this k3d build's default k3s image.
	if target.Image != "" {
		compare("image", []string{normalizeImageRef(actual.Image)}, []string{normalizeImageRef(target.Image)})
	}

	compare("volumes", sortedSet(userScopeVolumes(actual.Volumes)), sortedSet(target.Volumes))

	curArgs, curNodeLabels := userScopeArgs(actual)
	// The target node comes straight from the transform: Args holds the
	// user args, K3sNodeLabels the labels — no unwrapping needed.
	compare("k3sArgs", sortedSet(curArgs), sortedSet(target.Args))
	compare("k3sNodeLabels", labelsToSortedList(curNodeLabels), labelsToSortedList(target.K3sNodeLabels))

	compare("env", sortedSet(userScopeEnv(actual.Env)), sortedSet(userScopeEnv(target.Env)))

	compare("memory", []string{normalizeMemory(actual.Memory)}, []string{normalizeMemory(target.Memory)})

	if len(diff.Changes) > 0 {
		diff.Changeset = &NodeSpecChangeset{
			Image:         target.Image,
			Volumes:       append([]string{}, target.Volumes...),
			Args:          append([]string{}, target.Args...),
			Env:           userScopeEnv(target.Env),
			K3sNodeLabels: target.K3sNodeLabels,
			RuntimeLabels: target.RuntimeLabels,
			Memory:        target.Memory,
			Files:         append([]k3d.File{}, target.Files...),
		}
	}
	return diff
}

// portMapToSortedList renders a nat.PortMap-ish node port set as sorted
// "port->hostip:hostport" strings, optionally dropping the kube-api port
// (its host side is frequently auto-assigned at create time).
func portMapToSortedList(node *k3d.Node, dropAPIPort bool) []string {
	out := []string{}
	for port, bindings := range node.Ports {
		if dropAPIPort && port.Port() == k3d.DefaultAPIPort {
			continue
		}
		for _, b := range bindings {
			out = append(out, fmt.Sprintf("%s->%s:%s", port, b.HostIP, b.HostPort))
		}
	}
	sort.Strings(out)
	return out
}

// targetKubeAPIHostPort safely extracts the desired kube-api host port from
// a target cluster spec (KubeAPI may be unset).
func targetKubeAPIHostPort(target *k3d.Cluster) string {
	if target.KubeAPI == nil {
		return ""
	}
	return target.KubeAPI.Binding.HostPort
}

// nodeAPIPortBinding returns the port-map key and the first host binding of
// the kube-api port on a node (usually the serverlb). The key is matched on
// the port number only, because the target side uses the bare "6443" key
// (LoadbalancerPrepare) while reconstructed nodes carry the
// runtime-normalized "6443/tcp".
func nodeAPIPortBinding(node *k3d.Node) (nat.Port, nat.PortBinding, bool) {
	for port, bindings := range node.Ports {
		if port.Port() == k3d.DefaultAPIPort && len(bindings) > 0 {
			return port, bindings[0], true
		}
	}
	return "", nat.PortBinding{}, false
}

// diffStringSets returns the entries only present in desired (added) and
// only present in current (removed). Inputs are the sorted-set renderings
// used for field comparison; outputs keep that order.
func diffStringSets(current, desired []string) (added, removed []string) {
	curSet := map[string]struct{}{}
	for _, s := range current {
		curSet[s] = struct{}{}
	}
	desSet := map[string]struct{}{}
	for _, s := range desired {
		desSet[s] = struct{}{}
	}
	for _, s := range desired {
		if _, ok := curSet[s]; !ok {
			added = append(added, s)
		}
	}
	for _, s := range current {
		if _, ok := desSet[s]; !ok {
			removed = append(removed, s)
		}
	}
	return added, removed
}

// diffLoadBalancerSpec diffs the serverlb's host port mappings (and the
// explicitly requested kube-api host port) against the desired state and
// returns the changeset for the loadbalancer replacement, or nil if the
// loadbalancer already matches.
//
// Only the host port bindings are diffed. The proxied-upstream lists inside
// the LB config are NOT compared: they are rewritten from the config's
// desired state whenever the loadbalancer is replaced, but pure upstream
// drift without a port change is not detected (UpdateLoadbalancerConfig
// regenerates them all-servers anyway after every server replacement).
func diffLoadBalancerSpec(targetLB, actualLB *k3d.Node, targetKubeAPI *k3d.ExposureOpts) *LoadBalancerSpecDiff {
	added, removed := diffStringSets(portMapToSortedList(actualLB, true), portMapToSortedList(targetLB, true))

	actualAPIKey, actualAPIBinding, hasActualAPI := nodeAPIPortBinding(actualLB)
	desiredHP := ""
	if targetKubeAPI != nil {
		desiredHP = targetKubeAPI.Binding.HostPort
	}
	var kubeAPIDiff *FieldDiff
	if desiredHP != "" && desiredHP != "random" && hasActualAPI && actualAPIBinding.HostPort != desiredHP {
		kubeAPIDiff = &FieldDiff{
			Field:   "kube-api host port",
			Current: []string{actualAPIBinding.HostPort},
			Desired: []string{desiredHP},
		}
	}

	if len(added) == 0 && len(removed) == 0 && kubeAPIDiff == nil {
		return nil
	}

	// Desired port set for the replacement: the config's user port mappings
	// plus the kube-api binding. The kube-api binding keeps the actual
	// HostIP (HostIP is not diffed) and, unless explicitly changed, the
	// actual HostPort (which may have been auto-assigned at create time).
	desired := nat.PortMap{}
	for port, bindings := range targetLB.Ports {
		if port.Port() == k3d.DefaultAPIPort {
			continue
		}
		desired[port] = append([]nat.PortBinding{}, bindings...)
	}
	if hasActualAPI {
		apiBinding := actualAPIBinding
		if kubeAPIDiff != nil {
			apiBinding.HostPort = desiredHP
		}
		desired[actualAPIKey] = []nat.PortBinding{apiBinding}
	}

	return &LoadBalancerSpecDiff{
		NodeName:     actualLB.Name,
		Added:        added,
		Removed:      removed,
		KubeAPI:      kubeAPIDiff,
		DesiredPorts: desired,
	}
}

// DiffClusterSpec diffs a desired cluster config (as produced by the
// standard config transform pipeline) against the actual cluster state.
func DiffClusterSpec(ctx context.Context, runtime k3drt.Runtime, actual *k3d.Cluster, clusterConfig *conf.ClusterConfig) (*ClusterSpecDiff, error) {
	target := &clusterConfig.Cluster
	diff := &ClusterSpecDiff{}

	if target.Name != actual.Name {
		return nil, fmt.Errorf("config is for cluster '%s', but reconfiguring cluster '%s'", target.Name, actual.Name)
	}

	/* Cluster-level: definite mismatches that need a full recreate */

	if target.Network.Name != "" && actual.Network.Name != "" && target.Network.Name != actual.Network.Name {
		diff.Unsupported = append(diff.Unsupported, fmt.Sprintf("network: cluster runs in '%s', config wants '%s' (requires cluster recreate)", actual.Network.Name, target.Network.Name))
	}

	if target.Network.IPAM.Managed && target.Network.IPAM.IPPrefix.IsValid() {
		if net, err := runtime.GetNetwork(ctx, &k3d.ClusterNetwork{Name: actual.Network.Name}); err == nil && net.IPAM.IPPrefix.IsValid() {
			if net.IPAM.IPPrefix != target.Network.IPAM.IPPrefix {
				diff.Unsupported = append(diff.Unsupported, fmt.Sprintf("subnet: cluster network uses %s, config wants %s (requires cluster recreate)", net.IPAM.IPPrefix, target.Network.IPAM.IPPrefix))
			}
		} else {
			diff.NotDiffable = append(diff.NotDiffable, "subnet: could not inspect the actual cluster network for comparison")
		}
	}

	if target.Token != "" && actual.Token != "" && target.Token != actual.Token {
		diff.Unsupported = append(diff.Unsupported, "token: config specifies a different cluster token (requires cluster recreate)")
	}

	targetNodes := map[string]*k3d.Node{}
	targetServers, targetAgents := 0, 0
	var targetLB *k3d.Node
	for _, n := range target.Nodes {
		switch n.Role {
		case k3d.ServerRole:
			targetServers++
			targetNodes[n.Name] = n
		case k3d.AgentRole:
			targetAgents++
			targetNodes[n.Name] = n
		case k3d.LoadBalancerRole:
			targetLB = n
		}
	}

	actualServers, actualAgents := 0, 0
	var actualLB *k3d.Node
	actualWorkers := []*k3d.Node{}
	for _, n := range actual.Nodes {
		switch n.Role {
		case k3d.ServerRole:
			actualServers++
			actualWorkers = append(actualWorkers, n)
		case k3d.AgentRole:
			actualAgents++
			actualWorkers = append(actualWorkers, n)
		case k3d.LoadBalancerRole:
			actualLB = n
		}
	}

	if targetServers != actualServers || targetAgents != actualAgents {
		diff.Unsupported = append(diff.Unsupported, fmt.Sprintf("node count: cluster has %d servers / %d agents, config wants %d / %d (add/remove nodes with `k3d node create/delete` or recreate the cluster)", actualServers, actualAgents, targetServers, targetAgents))
	}

	if (targetLB == nil) != (actualLB == nil) {
		diff.Unsupported = append(diff.Unsupported, "loadbalancer: presence differs between config and cluster (requires cluster recreate)")
	} else if targetLB != nil && actualLB != nil {
		// Port-mapping differences (incl. the kube-api host port) are
		// applied by replacing only the loadbalancer container.
		diff.LoadBalancer = diffLoadBalancerSpec(targetLB, actualLB, target.KubeAPI)
	}

	// Without a serverlb, the kube-api host binding is published on the
	// server container itself; changing it there still requires a recreate.
	// With a serverlb the binding lives on the LB and is handled above —
	// deliberately NOT compared against the servers' k3d.server.api.port
	// labels, which are immutable on running containers and go stale after
	// an LB-only port change.
	if actualLB == nil {
		if hp := targetKubeAPIHostPort(target); hp != "" && hp != "random" {
			actualHP := ""
			for _, n := range actualWorkers {
				if n.Role == k3d.ServerRole && n.ServerOpts.KubeAPI != nil {
					actualHP = n.ServerOpts.KubeAPI.Binding.HostPort
					break
				}
			}
			if actualHP != "" && actualHP != hp {
				diff.Unsupported = append(diff.Unsupported, fmt.Sprintf("exposeAPI: kube-api host port is %s, config wants %s (requires cluster recreate)", actualHP, hp))
			}
		}
	}

	/* Cluster-level: aspects we cannot compare — reported, not ignored */

	if clusterConfig.ClusterCreateOpts.Registries.Create != nil || len(clusterConfig.ClusterCreateOpts.Registries.Use) > 0 || clusterConfig.ClusterCreateOpts.Registries.Config != nil {
		diff.NotDiffable = append(diff.NotDiffable, "registries: not diffed by reconfigure (registry setup is only evaluated at cluster creation)")
	}
	if len(clusterConfig.ClusterCreateOpts.HostAliases) > 0 {
		diff.NotDiffable = append(diff.NotDiffable, "hostAliases: drift is not detected (aliases are injected into /etc/hosts at start time)")
	}
	if clusterConfig.ClusterCreateOpts.GPURequest != "" {
		diff.NotDiffable = append(diff.NotDiffable, "gpus: not reconstructable from a running container, drift is not detected")
	}

	perNodeNotDiffable := map[string]bool{}
	for _, n := range target.Nodes {
		if len(n.RuntimeUlimits) > 0 {
			perNodeNotDiffable["runtimeUlimits"] = true
		}
		if n.HostPidMode {
			perNodeNotDiffable["hostPidMode"] = true
		}
		if len(n.RuntimeLabels) > 0 {
			perNodeNotDiffable["runtimeLabels"] = true
		}
		if len(n.Files) > 0 {
			perNodeNotDiffable["files"] = true
		}
	}
	if perNodeNotDiffable["runtimeUlimits"] {
		diff.NotDiffable = append(diff.NotDiffable, "runtimeUlimits: not reconstructable from a running container, drift is not detected")
	}
	if perNodeNotDiffable["hostPidMode"] {
		diff.NotDiffable = append(diff.NotDiffable, "hostPidMode: not reconstructable from a running container, drift is not detected")
	}
	if perNodeNotDiffable["runtimeLabels"] {
		diff.NotDiffable = append(diff.NotDiffable, "runtime labels: applied to nodes when they are replaced for other reasons; drift is not detected")
	}
	if perNodeNotDiffable["files"] {
		diff.NotDiffable = append(diff.NotDiffable, "files: (re)written on nodes when they are replaced for other reasons; content drift is not detected")
	}

	/* Per-node diff */

	for _, actualNode := range actualWorkers {
		targetNode, found := targetNodes[actualNode.Name]
		if !found {
			// covered by the node-count mismatch above; skip silently here
			continue
		}
		nodeDiff := diffNodeSpec(targetNode, actualNode)
		if len(nodeDiff.Changes) > 0 {
			diff.ChangedNodes = append(diff.ChangedNodes, nodeDiff)
		} else {
			diff.UnchangedNodes = append(diff.UnchangedNodes, actualNode.Name)
		}
	}

	return diff, nil
}

// Render writes a human-readable representation of the diff to the log.
// dryRun only changes the phrasing ("would be" vs "will be").
func (d *ClusterSpecDiff) Render(dryRun bool) {
	for _, entry := range d.Unsupported {
		l.Log().Errorf("UNSUPPORTED %s", entry)
	}
	for _, entry := range d.NotDiffable {
		l.Log().Warnf("NOT DIFFED  %s", entry)
	}
	verb := "will"
	if dryRun {
		verb = "would"
	}
	for _, nd := range d.ChangedNodes {
		l.Log().Infof("~ %s %s %s be replaced:", nd.Role, nd.NodeName, verb)
		for _, ch := range nd.Changes {
			l.Log().Infof("    %-13s %v -> %v", ch.Field+":", ch.Current, ch.Desired)
		}
	}
	if lb := d.LoadBalancer; lb != nil {
		l.Log().Infof("~ loadbalancer %s %s be replaced:", lb.NodeName, verb)
		for _, m := range lb.Added {
			l.Log().Infof("    + port %s (added)", m)
		}
		for _, m := range lb.Removed {
			l.Log().Infof("    - port %s (removed)", m)
		}
		if lb.KubeAPI != nil {
			l.Log().Infof("    ~ %s: %v -> %v (default kubeconfig %s be updated)", lb.KubeAPI.Field, lb.KubeAPI.Current, lb.KubeAPI.Desired, verb)
		}
	}
	for _, name := range d.UnchangedNodes {
		l.Log().Infof("= %s unchanged", name)
	}
}

// ClusterReconfigureFromConfig diffs the desired cluster config against the
// actual cluster and rolls every differing node through the rolling-replace
// machinery (same semantics as `reconfigure --image`: ordering, drain, etcd
// rotation, --force for single-server). Unchanged nodes are skipped.
func ClusterReconfigureFromConfig(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, clusterConfig *conf.ClusterConfig, dryRun bool, opts ClusterReconfigureOpts) error {
	diff, err := DiffClusterSpec(ctx, runtime, cluster, clusterConfig)
	if err != nil {
		return err
	}

	diff.Render(dryRun)

	if dryRun {
		lbNote := ""
		if diff.LoadBalancer != nil {
			lbNote = ", loadbalancer would be replaced"
		}
		l.Log().Infof("Dry run: %d node(s) would be replaced, %d unchanged, %d unsupported difference(s)%s", len(diff.ChangedNodes), len(diff.UnchangedNodes), len(diff.Unsupported), lbNote)
		return nil
	}

	if len(diff.Unsupported) > 0 {
		return fmt.Errorf("config contains %d change(s) that cannot be applied by replacing nodes: %s", len(diff.Unsupported), strings.Join(diff.Unsupported, "; "))
	}

	if len(diff.ChangedNodes) == 0 && diff.LoadBalancer == nil {
		l.Log().Infof("Cluster '%s' already matches the given config — nothing to do", cluster.Name)
		return nil
	}

	// Apply the loadbalancer port changes first: a single fast container
	// replacement that fails early before the (long) node rolls. Order is
	// otherwise irrelevant — node rolls neither depend on the LB's host
	// ports nor change the node names its config references.
	if diff.LoadBalancer != nil {
		if err := applyLoadBalancerSpec(ctx, runtime, cluster, clusterConfig, diff.LoadBalancer); err != nil {
			return fmt.Errorf("failed to replace loadbalancer of cluster '%s' with updated port mappings: %w", cluster.Name, err)
		}
	}

	if len(diff.ChangedNodes) == 0 {
		return nil
	}

	changesets := map[string]*NodeSpecChangeset{}
	for _, nd := range diff.ChangedNodes {
		changesets[nd.NodeName] = nd.Changeset
	}

	op := makeReplaceOpFn(func(node *k3d.Node) *NodeEditChangeset {
		return &NodeEditChangeset{Spec: changesets[node.Name]}
	})

	if err := RollingApply(ctx, runtime, cluster, RollingApplyOpts{
		Verb:         "Reconfiguring",
		Force:        opts.Force,
		DrainTimeout: opts.DrainTimeout,
		ReadyTimeout: opts.ReadyTimeout,
		Op:           op,
		Filter: func(node *k3d.Node) bool {
			_, changed := changesets[node.Name]
			return changed
		},
	}); err != nil {
		return fmt.Errorf("rolling reconfigure of cluster '%s' failed and is NOT automatically resumable — the cluster may be left with mixed node specs and, in HA, a half-rotated etcd membership (see `k3d cluster reconfigure --help` for recovery): %w", cluster.Name, err)
	}
	return nil
}

// applyLoadBalancerSpec replaces the serverlb container with the desired
// port set and the transform-produced LB config (which carries the correct
// proxied-upstream targets from the config's node filters). If the kube-api
// host port changed, the default kubeconfig is refreshed afterwards so
// `kubectl` keeps working; externally saved kubeconfig copies are not
// touched and must be refreshed via `k3d kubeconfig get/merge`.
func applyLoadBalancerSpec(ctx context.Context, runtime k3drt.Runtime, cluster *k3d.Cluster, clusterConfig *conf.ClusterConfig, lbDiff *LoadBalancerSpecDiff) error {
	var actualLBNode *k3d.Node
	for _, n := range cluster.Nodes {
		if n.Role == k3d.LoadBalancerRole {
			actualLBNode = n
			break
		}
	}
	if actualLBNode == nil {
		return fmt.Errorf("no loadbalancer node found in cluster despite a loadbalancer port diff")
	}

	if clusterConfig.Cluster.ServerLoadBalancer == nil || clusterConfig.Cluster.ServerLoadBalancer.Config == nil {
		return fmt.Errorf("desired cluster config carries no loadbalancer config despite a loadbalancer port diff")
	}
	desiredConfig := clusterConfig.Cluster.ServerLoadBalancer.Config

	l.Log().Infof("Replacing loadbalancer %s with updated port mappings (%d added, %d removed)...", lbDiff.NodeName, len(lbDiff.Added), len(lbDiff.Removed))
	if err := replaceLoadbalancer(ctx, runtime, actualLBNode, lbDiff.DesiredPorts, desiredConfig); err != nil {
		return err
	}

	if lbDiff.KubeAPI != nil {
		// The servers' k3d.server.api.port labels still hold the old port
		// (container labels are immutable); KubeconfigGet reads the actual
		// LB binding instead, so the refreshed kubeconfig gets the new port.
		if output, err := KubeconfigGetWrite(ctx, runtime, cluster, "", &WriteKubeConfigOptions{UpdateExisting: true, UpdateCurrentContext: false}); err != nil {
			l.Log().Warnf("kube-api host port changed, but updating the default kubeconfig failed: %v — run `k3d kubeconfig merge %s -d` manually", err, cluster.Name)
		} else {
			l.Log().Infof("Updated kubeconfig '%s' with the new kube-api port %v", output, lbDiff.KubeAPI.Desired)
		}
	}

	return nil
}

// applySpecChangeset rebuilds the user-scope portion of a node copy from
// the desired spec while preserving the k3d-managed portion. The inverse of
// the userScope* projections above — keep both sides in sync, idempotency
// of `reconfigure -c` depends on it.
func applySpecChangeset(runtime k3drt.Runtime, result *k3d.Node, spec *NodeSpecChangeset) {
	if spec.Image != "" {
		result.Image = spec.Image
	}

	// Volumes: managed mounts (image volume, adopted anonymous volumes)
	// survive; meminfo/edac fakes are regenerated by NodeCreate from Memory.
	result.Volumes = append(managedVolumes(result.Volumes), spec.Volumes...)
	result.Memory = spec.Memory

	// Env: keep the join-state vars verbatim, replace the user scope.
	// Image-provided and NodeCreate-default vars are re-established by the
	// image / NodeCreate respectively.
	result.Env = append(preservedEnv(result.Env), spec.Env...)

	// Cmd/Args: rebuild from scratch. NodeCreate re-injects the role token
	// (patchServerSpec/patchAgentSpec on nil Cmd), the --tls-san pairs
	// (from the preserved labels) and --node-label pairs (from
	// K3sNodeLabels). --cluster-init is intentionally never re-added — a
	// replaced init server rejoins as a regular peer.
	result.Cmd = nil
	result.Args = append([]string{}, spec.Args...)
	result.K3sNodeLabels = spec.K3sNodeLabels

	// Runtime labels from the config are merged on top of the preserved
	// k3d-managed labels (the k3d.* set is authoritative and never
	// overridden by user labels — user label keys must not use the k3d
	// prefix anyway).
	for k, v := range spec.RuntimeLabels {
		if strings.HasPrefix(k, "k3d") {
			continue
		}
		if result.RuntimeLabels == nil {
			result.RuntimeLabels = map[string]string{}
		}
		result.RuntimeLabels[k] = v
	}

	// Files: rewrite via the same PreStart hook cluster creation uses
	// (NodeReplace forwards HookActions into NodeStart).
	result.Files = append([]k3d.File{}, spec.Files...)
	for _, file := range spec.Files {
		result.HookActions = append(result.HookActions, k3d.NodeHook{
			Stage: k3d.LifecycleStagePreStart,
			Action: actions.WriteFileAction{
				Runtime:     runtime,
				Content:     file.Content,
				Dest:        file.Destination,
				Mode:        0644,
				Description: file.Description,
			},
		})
	}
}
