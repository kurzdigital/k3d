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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	l "github.com/k3d-io/k3d/v5/pkg/logger"
	runtimeErr "github.com/k3d-io/k3d/v5/pkg/runtimes/errors"
	runtimeTypes "github.com/k3d-io/k3d/v5/pkg/runtimes/types"

	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

// CreateNode creates a new container
func (d Docker) CreateNode(ctx context.Context, node *k3d.Node) error {
	// translate node spec to docker container specs
	dockerNode, err := TranslateNodeToContainer(node)
	if err != nil {
		return fmt.Errorf("failed to translate k3d node spec to docker container spec: %w", err)
	}

	// create node
	_, err = createContainer(ctx, dockerNode, node.Name)
	if err != nil {
		return fmt.Errorf("failed to create container for node '%s': %w", node.Name, err)
	}

	return nil
}

// DeleteNode deletes a node
func (d Docker) DeleteNode(ctx context.Context, nodeSpec *k3d.Node) error {
	l.Log().Debugf("Deleting node %s ...", nodeSpec.Name)
	return removeContainer(ctx, nodeSpec.Name)
}

// GetNodesByLabel returns a list of existing nodes
func (d Docker) GetNodesByLabel(ctx context.Context, labels map[string]string) ([]*k3d.Node, error) {
	// (0) get containers
	containers, err := getContainersByLabel(ctx, labels)
	if err != nil {
		return nil, fmt.Errorf("docker failed to get containers with labels '%v': %w", labels, err)
	}

	// (1) convert them to node structs
	nodes := []*k3d.Node{}
	for _, container := range containers {
		var node *k3d.Node
		var err error

		containerDetails, err := getContainerDetails(ctx, container.ID)
		if err != nil {
			l.Log().Warnf("Failed to get details for container %s", container.Names[0])
			node, err = TranslateContainerToNode(&container)
			if err != nil {
				return nil, fmt.Errorf("failed to translate container '%s' to k3d node spec: %w", container.Names[0], err)
			}
		} else {
			node, err = TranslateContainerDetailsToNode(containerDetails)
			if err != nil {
				return nil, fmt.Errorf("failed to translate container'%s' details to k3d node spec: %w", containerDetails.Name, err)
			}
		}
		nodes = append(nodes, node)
	}

	return nodes, nil
}

// StartNode starts an existing node
func (d Docker) StartNode(ctx context.Context, node *k3d.Node) error {
	// (0) create docker client
	docker, err := GetDockerClient()
	if err != nil {
		return fmt.Errorf("failed to create docker client. %w", err)
	}
	defer docker.Close()

	// get container which represents the node
	nodeContainer, err := getNodeContainer(ctx, node)
	if err != nil {
		return fmt.Errorf("failed to get container for node '%s': %w", node.Name, err)
	}

	// check if the container is actually managed by
	if v, ok := nodeContainer.Labels["app"]; !ok || v != "k3d" {
		return fmt.Errorf("Failed to determine if container '%s' is managed by k3d (needs label 'app=k3d')", nodeContainer.ID)
	}

	// actually start the container
	l.Log().Infof("Starting node '%s'", node.Name)
	if err := docker.ContainerStart(ctx, nodeContainer.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("docker failed to start container for node '%s': %w", node.Name, err)
	}

	// get container which represents the node
	nodeContainerJSON, err := docker.ContainerInspect(ctx, nodeContainer.ID)
	if err != nil {
		return fmt.Errorf("Failed to inspect container %s for node %s: %+v", node.Name, nodeContainer.ID, err)
	}

	node.Created = nodeContainerJSON.Created
	node.State.Running = nodeContainerJSON.State.Running
	node.State.Started = nodeContainerJSON.State.StartedAt

	return nil
}

// StopNode stops an existing node
func (d Docker) StopNode(ctx context.Context, node *k3d.Node) error {
	// (0) create docker client
	docker, err := GetDockerClient()
	if err != nil {
		return fmt.Errorf("Failed to create docker client. %+v", err)
	}
	defer docker.Close()

	// get container which represents the node
	nodeContainer, err := getNodeContainer(ctx, node)
	if err != nil {
		return fmt.Errorf("failed to get container for node '%s': %w", node.Name, err)
	}

	// check if the container is actually managed by
	if v, ok := nodeContainer.Labels["app"]; !ok || v != "k3d" {
		return fmt.Errorf("Failed to determine if container '%s' is managed by k3d (needs label 'app=k3d')", nodeContainer.ID)
	}

	// actually stop the container
	if err := docker.ContainerStop(ctx, nodeContainer.ID, container.StopOptions{}); err != nil {
		return fmt.Errorf("docker failed to stop the container '%s': %w", nodeContainer.ID, err)
	}

	return nil
}

func getContainersByLabel(ctx context.Context, labels map[string]string) ([]types.Container, error) {
	// (0) create docker client
	docker, err := GetDockerClient()
	if err != nil {
		return nil, fmt.Errorf("Failed to create docker client. %+v", err)
	}
	defer docker.Close()

	// (1) list containers which have the default k3d labels attached
	filters := filters.NewArgs()
	for k, v := range k3d.DefaultRuntimeLabels {
		filters.Add("label", fmt.Sprintf("%s=%s", k, v))
	}
	for k, v := range labels {
		filters.Add("label", fmt.Sprintf("%s=%s", k, v))
	}

	containers, err := docker.ContainerList(ctx, container.ListOptions{
		Filters: filters,
		All:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	return containers, nil
}

// getContainerDetails returns the containerjson with more details
func getContainerDetails(ctx context.Context, containerID string) (types.ContainerJSON, error) {
	// (0) create docker client
	docker, err := GetDockerClient()
	if err != nil {
		return types.ContainerJSON{}, fmt.Errorf("failed to create docker client. %w", err)
	}
	defer docker.Close()

	containerDetails, err := docker.ContainerInspect(ctx, containerID)
	if err != nil {
		return types.ContainerJSON{}, fmt.Errorf("failed to get details for container '%s': %w", containerID, err)
	}

	return containerDetails, nil
}

// GetNode tries to get a node container by its name
func (d Docker) GetNode(ctx context.Context, node *k3d.Node) (*k3d.Node, error) {
	container, err := getNodeContainer(ctx, node)
	if err != nil {
		return node, fmt.Errorf("failed to get container for node '%s': %w", node.Name, err)
	}

	containerDetails, err := getContainerDetails(ctx, container.ID)
	if err != nil {
		return node, fmt.Errorf("failed to get details for container '%s': %w", container.ID, err)
	}

	node, err = TranslateContainerDetailsToNode(containerDetails)
	if err != nil {
		return node, fmt.Errorf("failed to translate container '%s' details to node spec: %w", containerDetails.Name, err)
	}

	return node, nil
}

// GetNodeStatus returns the status of a node (Running, Started, etc.)
func (d Docker) GetNodeStatus(ctx context.Context, node *k3d.Node) (bool, string, error) {
	stateString := ""
	running := false

	// get the container for the given node
	container, err := getNodeContainer(ctx, node)
	if err != nil {
		return running, stateString, fmt.Errorf("failed to get container for node '%s': %w", node.Name, err)
	}

	// create docker client
	docker, err := GetDockerClient()
	if err != nil {
		return running, stateString, fmt.Errorf("failed to get docker client: %w", err)
	}
	defer docker.Close()

	containerInspectResponse, err := docker.ContainerInspect(ctx, container.ID)
	if err != nil {
		return running, stateString, fmt.Errorf("docker failed to inspect container '%s': %w", container.ID, err)
	}

	running = containerInspectResponse.ContainerJSONBase.State.Running
	stateString = containerInspectResponse.ContainerJSONBase.State.Status

	return running, stateString, nil
}

// NodeIsRunning tells the caller if a given node is in "running" state
func (d Docker) NodeIsRunning(ctx context.Context, node *k3d.Node) (bool, error) {
	isRunning, _, err := d.GetNodeStatus(ctx, node)
	if err != nil {
		return false, fmt.Errorf("failed to get status for node '%s': %w", node.Name, err)
	}
	return isRunning, nil
}

// GetNodeLogs returns the logs from a given node
func (d Docker) GetNodeLogs(ctx context.Context, node *k3d.Node, since time.Time, opts *runtimeTypes.NodeLogsOpts) (io.ReadCloser, error) {
	// get the nodeContainer for the given node
	nodeContainer, err := getNodeContainer(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to get container for node '%s': %w", node.Name, err)
	}

	// create docker client
	docker, err := GetDockerClient()
	if err != nil {
		return nil, fmt.Errorf("failed to get docker client; %w", err)
	}
	defer docker.Close()

	containerInspectResponse, err := docker.ContainerInspect(ctx, nodeContainer.ID)
	if err != nil {
		return nil, fmt.Errorf("failed ton inspect container '%s': %w", nodeContainer.ID, err)
	}

	if !containerInspectResponse.ContainerJSONBase.State.Running {
		return nil, fmt.Errorf("node '%s' (container '%s') not running", node.Name, containerInspectResponse.ID)
	}

	sinceStr := ""
	if !since.IsZero() {
		sinceStr = since.Format("2006-01-02T15:04:05.999999999Z")
	}
	logreader, err := docker.ContainerLogs(ctx, nodeContainer.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true, Since: sinceStr, Follow: opts.Follow})
	if err != nil {
		return nil, fmt.Errorf("docker failed to get logs from node '%s' (container '%s'): %w", node.Name, nodeContainer.ID, err)
	}

	return logreader, nil
}

// ExecInNodeGetLogs executes a command inside a node and returns the logs to the caller, e.g. to parse them
func (d Docker) ExecInNodeGetLogs(ctx context.Context, node *k3d.Node, cmd []string) (*bufio.Reader, error) {
	logs, err := executeInNode(ctx, node, cmd, nil)
	if logs == nil {
		return nil, err
	}
	return bufio.NewReader(bytes.NewReader(logs)), err
}

// GetImageStream creates a tar stream for the given images, to be read (and closed) by the caller
func (d Docker) GetImageStream(ctx context.Context, image []string) (io.ReadCloser, error) {
	docker, err := GetDockerClient()
	if err != nil {
		return nil, err
	}
	reader, err := docker.ImageSave(ctx, image)
	return reader, err
}

// ExecInNode execs a command inside a node
func (d Docker) ExecInNode(ctx context.Context, node *k3d.Node, cmd []string) error {
	return execInNode(ctx, node, cmd, nil)
}

func execInNode(ctx context.Context, node *k3d.Node, cmd []string, stdin io.ReadCloser) error {
	logs, err := executeInNode(ctx, node, cmd, stdin)
	if err != nil && len(logs) > 0 {
		err = fmt.Errorf("%w: Logs from failed access process:\n%s", err, string(logs))
	}
	return err
}

func (d Docker) ExecInNodeWithStdin(ctx context.Context, node *k3d.Node, cmd []string, stdin io.ReadCloser) error {
	return execInNode(ctx, node, cmd, stdin)
}

// executeInNode runs cmd inside node's container, returning the captured
// stdout/stderr bytes alongside any error.
//
// The hijacked stream from ContainerExecAttach is drained concurrently
// with the Inspect-poll loop in a dedicated goroutine. Without that
// drain, Docker engines that serialise per-container state access can
// hold the container's state lock as long as the hijacked stream sits
// unread, which makes ContainerExecInspect block until the outer
// context expires — even for trivially short commands. This was
// observed under daemon load, when many exec invocations land
// back-to-back on the same daemon.
func executeInNode(ctx context.Context, node *k3d.Node, cmd []string, stdin io.ReadCloser) ([]byte, error) {
	l.Log().Debugf("Executing command '%+v' in node '%s'", cmd, node.Name)

	// get the container for the given node
	nodeContainer, err := getNodeContainer(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("failed to get container for node '%s': %w", node.Name, err)
	}

	// create docker client
	docker, err := GetDockerClient()
	if err != nil {
		return nil, fmt.Errorf("failed to get docker client: %w", err)
	}
	defer docker.Close()

	attachStdin := false
	if stdin != nil {
		attachStdin = true
	}

	// exec
	exec, err := docker.ContainerExecCreate(ctx, nodeContainer.ID, container.ExecOptions{
		Privileged: true,
		// Don't use tty true when piping stdin.
		Tty:          !attachStdin,
		AttachStderr: true,
		AttachStdout: true,
		AttachStdin:  attachStdin,
		Cmd:          cmd,
	})
	if err != nil {
		return nil, fmt.Errorf("docker failed to create exec config for node '%s': %+v", node.Name, err)
	}

	execConnection, err := docker.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{
		// Don't use tty true when piping stdin.
		Tty: !attachStdin,
	})
	if err != nil {
		return nil, fmt.Errorf("docker failed to attach to exec process in node '%s': %w", node.Name, err)
	}
	defer execConnection.Close()

	// Drain stdout/stderr concurrently with the inspect-loop. See function doc.
	type drainResult struct {
		bytes []byte
		err   error
	}
	drainCh := make(chan drainResult, 1)
	go func() {
		b, drainErr := io.ReadAll(execConnection.Reader)
		drainCh <- drainResult{bytes: b, err: drainErr}
	}()

	// Pump stdin in another goroutine, if requested.
	if stdin != nil {
		go func() {
			if _, err := io.Copy(execConnection.Conn, stdin); err != nil {
				l.Log().Errorf("Failed to copy read stream. %v", err)
			}
			if err := stdin.Close(); err != nil {
				l.Log().Errorf("Failed to close stdin stream. %v", err)
			}
		}()
	}

	// abortAndCollectLogs is for error paths only: the daemon may keep the
	// exec stream open indefinitely, so close the connection first to
	// unblock the drain goroutine (io.ReadAll returns once the underlying
	// connection is closed), then collect whatever was read. Any drain
	// error is ignored here because the caller already has a primary error
	// to report and the logs are merely supplementary.
	abortAndCollectLogs := func() []byte {
		execConnection.Close()
		res := <-drainCh
		return res.bytes
	}

	for {
		// get info about exec process inside container
		execInfo, inspectErr := docker.ContainerExecInspect(ctx, exec.ID)
		if inspectErr != nil {
			return abortAndCollectLogs(), fmt.Errorf("docker failed to inspect exec process in node '%s': %w", node.Name, inspectErr)
		}

		// if still running, continue loop (ctx-aware sleep so a cancelled
		// context returns promptly rather than after a full second)
		if execInfo.Running {
			l.Log().Tracef("Exec process '%+v' still running in node '%s'.. sleeping for 1 second...", cmd, node.Name)
			select {
			case <-ctx.Done():
				return abortAndCollectLogs(), ctx.Err()
			case <-time.After(1 * time.Second):
			}
			continue
		}

		// Process exited: the daemon closes its end of the stream, so the
		// drain goroutine finishes on its own; still honour ctx cancellation.
		var logs []byte
		select {
		case res := <-drainCh:
			if res.err != nil {
				return res.bytes, fmt.Errorf("error reading logs from exec process in node '%s': %w", node.Name, res.err)
			}
			logs = res.bytes
		case <-ctx.Done():
			return abortAndCollectLogs(), ctx.Err()
		}

		if execInfo.ExitCode == 0 { // success
			l.Log().Debugf("Exec process in node '%s' exited with '0'", node.Name)
			return logs, nil
		}
		return logs, fmt.Errorf("Exec process in node '%s' failed with exit code '%d'", node.Name, execInfo.ExitCode)
	}
}

// GetNodesInNetwork returns all the nodes connected to a given network
func (d Docker) GetNodesInNetwork(ctx context.Context, network string) ([]*k3d.Node, error) {
	// create docker client
	docker, err := GetDockerClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	defer docker.Close()

	net, err := GetNetwork(ctx, network)
	if err != nil {
		return nil, fmt.Errorf("failed to get network '%s': %w", network, err)
	}

	connectedNodes := []*k3d.Node{}

	// loop over list of containers connected to this cluster and transform them into nodes internally
	for cID := range net.Containers {
		containerDetails, err := getContainerDetails(ctx, cID)
		if err != nil {
			return nil, fmt.Errorf("docker failed to get details of container '%s': %w", cID, err)
		}
		node, err := TranslateContainerDetailsToNode(containerDetails)
		if err != nil {
			if errors.Is(err, runtimeErr.ErrRuntimeContainerUnknown) {
				l.Log().Tracef("GetNodesInNetwork: inspected non-k3d-managed container %s", containerDetails.Name)
				continue
			}
			return nil, fmt.Errorf("failed to translate container '%s' details to node spec: %w", containerDetails.Name, err)
		}
		connectedNodes = append(connectedNodes, node)
	}

	return connectedNodes, nil
}

func (d Docker) RenameNode(ctx context.Context, node *k3d.Node, newName string) error {
	// get the container for the given node
	container, err := getNodeContainer(ctx, node)
	if err != nil {
		return fmt.Errorf("failed to get container for node '%s': %w", node.Name, err)
	}

	// create docker client
	docker, err := GetDockerClient()
	if err != nil {
		return fmt.Errorf("failed to get docker client: %w", err)
	}
	defer docker.Close()

	return docker.ContainerRename(ctx, container.ID, newName)
}
