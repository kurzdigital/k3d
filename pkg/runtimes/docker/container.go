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
	"context"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	l "github.com/k3d-io/k3d/v5/pkg/logger"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
	"github.com/sirupsen/logrus"
)

// getNodeContainerLookupRetries / getNodeContainerLookupBackoff bound the
// short retry that getNodeContainer does on zero-result lookups. NodeReplace
// renames the old container, then creates the new one under the original
// name — for a sub-second window, the original name has zero containers
// attached. Callers iterating the cluster from another goroutine (or a
// stale snapshot) can otherwise hit "Didn't find container" mid-replace.
//
// 5 attempts with a 200ms sleep between them (i.e. 4 × 200ms = 800ms
// worst case) for a missing container that legitimately stays missing —
// small enough to not change the user-visible feedback when a node
// really is gone.
const (
	getNodeContainerLookupRetries = 5
	getNodeContainerLookupBackoff = 200 * time.Millisecond
)

// createContainer creates a new docker container from translated specs
func createContainer(ctx context.Context, dockerNode *NodeInDocker, name string) (string, error) {
	l.Log().Tracef("Creating docker container with translated config\n%+v\n", dockerNode)

	// initialize docker client
	docker, err := GetDockerClient()
	if err != nil {
		return "", fmt.Errorf("failed to create docker client: %w", err)
	}
	defer docker.Close()

	// create container
	var resp container.CreateResponse
	for {
		resp, err = docker.ContainerCreate(ctx, &dockerNode.ContainerConfig, &dockerNode.HostConfig, &dockerNode.NetworkingConfig, nil, name)
		if err != nil {
			if client.IsErrNotFound(err) {
				if err := pullImage(ctx, docker, dockerNode.ContainerConfig.Image); err != nil {
					return "", fmt.Errorf("docker failed to pull image '%s': %w", dockerNode.ContainerConfig.Image, err)
				}
				continue
			}
			return "", fmt.Errorf("docker failed to create container '%s': %w", name, err)
		}
		l.Log().Debugf("Created container %s (ID: %s)", name, resp.ID)
		break
	}

	return resp.ID, nil
}

func startContainer(ctx context.Context, ID string) error {
	// initialize docker client
	docker, err := GetDockerClient()
	if err != nil {
		return fmt.Errorf("failed to get docker client: %w", err)
	}
	defer docker.Close()

	return docker.ContainerStart(ctx, ID, container.StartOptions{})
}

// removeContainer deletes a running container (like docker rm -f)
func removeContainer(ctx context.Context, ID string) error {
	// (0) create docker client
	docker, err := GetDockerClient()
	if err != nil {
		return fmt.Errorf("failed to get docker client: %w", err)
	}
	defer docker.Close()

	// (1) define remove options
	options := container.RemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}

	// (2) remove container
	if err := docker.ContainerRemove(ctx, ID, options); err != nil {
		return fmt.Errorf("docker failed to remove the container '%s': %w", ID, err)
	}

	l.Log().Tracef("[Docker] Deleted Container %s", ID)

	return nil
}

// pullImage pulls a container image and outputs progress if --verbose flag is set
func pullImage(ctx context.Context, docker client.APIClient, image string) error {
	resp, err := docker.ImagePull(ctx, image, dockerimage.PullOptions{})
	if err != nil {
		return fmt.Errorf("docker failed to pull the image '%s': %w", image, err)
	}
	defer resp.Close()

	l.Log().Infof("Pulling image '%s'", image)

	// in debug mode (--verbose flag set), output pull progress
	var writer io.Writer = io.Discard
	if l.Log().GetLevel() == logrus.DebugLevel {
		writer = l.Log().Out
	}
	_, err = io.Copy(writer, resp)
	if err != nil {
		l.Log().Warnf("Couldn't get docker output: %v", err)
	}

	return nil
}

func getNodeContainer(ctx context.Context, node *k3d.Node) (*types.Container, error) {
	// (0) create docker client
	docker, err := GetDockerClient()
	if err != nil {
		return nil, fmt.Errorf("failed to get docker client: %w", err)
	}
	defer docker.Close()

	// (1) list containers which have the default k3d labels attached
	filters := filters.NewArgs()
	for k, v := range node.RuntimeLabels {
		filters.Add("label", fmt.Sprintf("%s=%s", k, v))
	}

	// regex filtering for exact name match
	// Assumptions:
	// -> container names start with a / (see https://github.com/moby/moby/issues/29997)
	// -> user input may or may not have the "k3d-" prefix
	filters.Add("name", fmt.Sprintf("^/?(%s-)?%s$", k3d.DefaultObjectNamePrefix, node.Name))

	// Bounded retry on zero-result lookups: NodeReplace renames the old
	// container before creating the new one under the original name, so a
	// caller racing that window briefly sees nothing at the original name.
	// Errors and ambiguous (>1) results return immediately — those are not
	// transient.
	for attempt := 0; attempt < getNodeContainerLookupRetries; attempt++ {
		containers, err := docker.ContainerList(ctx, container.ListOptions{
			Filters: filters,
			All:     true,
		})
		if err != nil {
			return nil, fmt.Errorf("Failed to list containers: %+v", err)
		}
		if len(containers) > 1 {
			return nil, fmt.Errorf("Failed to get a single container for name '%s'. Found: %d", node.Name, len(containers))
		}
		if len(containers) == 1 {
			return &containers[0], nil
		}

		if attempt == getNodeContainerLookupRetries-1 {
			break
		}
		l.Log().Tracef("getNodeContainer: 0 results for '%s' on attempt %d/%d (possible NodeReplace rename race), retrying in %s", node.Name, attempt+1, getNodeContainerLookupRetries, getNodeContainerLookupBackoff)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(getNodeContainerLookupBackoff):
		}
	}
	return nil, fmt.Errorf("Didn't find container for node '%s'", node.Name)
}

// executes an arbitrary command in a container while returning its exit code.
// useful to check something in docker env
func executeCheckInContainer(ctx context.Context, image string, cmd []string) (int64, error) {
	docker, err := GetDockerClient()
	if err != nil {
		return -1, fmt.Errorf("failed to create docker client: %w", err)
	}
	defer docker.Close()

	// create container
	var resp container.CreateResponse
	for {
		resp, err = docker.ContainerCreate(ctx, &container.Config{
			Image:      image,
			Cmd:        cmd,
			Tty:        false,
			Entrypoint: []string{},
		}, nil, nil, nil, "")
		if err != nil {
			if client.IsErrNotFound(err) {
				if err := pullImage(ctx, docker, image); err != nil {
					return -1, fmt.Errorf("docker failed to pull image '%s': %w", image, err)
				}
				continue
			}
			return -1, fmt.Errorf("docker failed to create container from image '%s' with cmd '%s': %w", image, cmd, err)
		}
		break
	}

	if err = startContainer(ctx, resp.ID); err != nil {
		return -1, fmt.Errorf("docker failed to start container from image '%s' with cmd '%s': %w", image, cmd, err)
	}

	exitCode := -1
	statusCh, errCh := docker.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return -1, fmt.Errorf("docker error while waiting for container '%s' to exit: %w", resp.ID, err)
		}
	case status := <-statusCh:
		exitCode = int(status.StatusCode)
	}

	if err = removeContainer(ctx, resp.ID); err != nil {
		return -1, fmt.Errorf("docker failed to remove container '%s': %w", resp.ID, err)
	}

	return int64(exitCode), nil
}

// CheckIfDirectoryExists checks for the existence of a given path inside the docker environment
func CheckIfDirectoryExists(ctx context.Context, image string, dir string) (bool, error) {
	l.Log().Tracef("checking if dir %s exists in docker environment...", dir)
	shellCmd := fmt.Sprintf("[ -d \"%s\" ] && exit 0 || exit 1", dir)
	cmd := []string{"sh", "-c", shellCmd}
	exitCode, err := executeCheckInContainer(ctx, image, cmd)
	l.Log().Tracef("check dir container returned %d exit code", exitCode)
	return exitCode == 0, err
}
