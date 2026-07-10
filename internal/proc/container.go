package proc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/adler32"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/docker/cli/cli/config"
	"github.com/kitproj/kit/internal/types"
	archive "github.com/moby/go-archive"
	"github.com/moby/moby/api/pkg/stdcopy"
	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"k8s.io/utils/strings/slices"
)

// legacyIndexServer is the auth config key Docker uses for images hosted on Docker Hub.
const legacyIndexServer = "https://index.docker.io/v1/"

type container struct {
	name string
	log  *log.Logger
	spec types.Spec
	cli  client.APIClient
	types.Task
	containerID string
}

func (c *container) Run(ctx context.Context, stdout, stderr io.Writer) error {

	log := c.log
	data, _ := json.Marshal(c.Task)
	expectedHash := fmt.Sprintf("%x", adler32.Checksum(data))

	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("failed to create docker client: %w", err)
	}
	c.cli = cli
	// close the client once the context is done rather than when Run returns:
	// the metrics goroutine keeps using c.cli until ctx is cancelled
	go func() {
		<-ctx.Done()
		cli.Close()
	}()

	dockerfile := filepath.Join(c.Image, "Dockerfile")
	id, existingHash, err := c.getContainer(ctx, cli)

	// If the container exists and the hash is different, remove it.
	if id != "" && existingHash != expectedHash {
		log.Println("removing container")
		if _, err := cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
			return fmt.Errorf("failed to remove container: %w", err)
		}
		id = ""
	}

	environ, err := types.Environ(c.spec, c.Task)
	if err != nil {
		return fmt.Errorf("error getting spec environ: %w", err)
	}

	if id != "" {
		log.Printf("container already exists, skipping build/pull\n")
	} else if _, err := os.Stat(dockerfile); err == nil {
		log.Printf("creating tar image from %q", dockerfile)
		r, err := archive.TarWithOptions(filepath.Dir(dockerfile), &archive.TarOptions{})
		if err != nil {
			return fmt.Errorf("failed to create tar: %w", err)
		}
		defer r.Close()
		log.Printf("building image from %q", dockerfile)
		resp, err := cli.ImageBuild(ctx, r, client.ImageBuildOptions{Dockerfile: filepath.Base(dockerfile), Tags: []string{c.name}})
		if err != nil {
			return fmt.Errorf("failed to build image: %w", err)
		}
		defer resp.Body.Close()
		log.Printf("building image from %q (logs)", dockerfile)
		if _, err = io.Copy(stdout, resp.Body); err != nil {
			return fmt.Errorf("failed to build image (logs): %w", err)
		}
	} else if c.ImagePullPolicy != "Never" {
		log.Printf("pulling image %q", c.Image)

		ref, err := reference.ParseNormalizedNamed(c.Image)
		if err != nil {
			return fmt.Errorf("unable to parse image: %w", err)
		}
		// Docker Hub credentials are stored under the legacy index server key,
		// other registries under their hostname.
		server := reference.Domain(ref)
		if server == "docker.io" {
			server = legacyIndexServer
		}
		errBuf := &bytes.Buffer{}
		cf := config.LoadDefaultConfigFile(errBuf)
		if errBuf.Len() > 0 {
			return fmt.Errorf("unable to load docker config: %s", errBuf.String())
		}
		authConfig, err := cf.GetAuthConfig(server)
		if err != nil {
			return fmt.Errorf("failed to get auth config: %w", err)
		}
		buf, err := json.Marshal(authConfig)
		if err != nil {
			return fmt.Errorf("failed to marshal auth config: %w", err)
		}
		encodedAuth := base64.URLEncoding.EncodeToString(buf)

		r, err := cli.ImagePull(ctx, c.Image, client.ImagePullOptions{
			RegistryAuth: encodedAuth,
		})
		if err != nil {
			return fmt.Errorf("failed to pull image: %w", err)
		}
		if _, err = io.Copy(stdout, r); err != nil {
			return fmt.Errorf("failed to pull image (logs): %w", err)
		}
		if err = r.Close(); err != nil {
			return fmt.Errorf("failed to pull image (close): %w", err)
		}
	}

	portSet, portBindings, err := c.createPorts()
	if err != nil {
		return fmt.Errorf("failed to create ports: %w", err)
	}
	binds, err := c.createBinds()
	if err != nil {
		return fmt.Errorf("failed to create binds: %w", err)
	}
	image := c.Image
	if _, err := os.Stat(filepath.Join(c.Image, "Dockerfile")); err == nil {
		image = c.name
	}

	log.Printf("creating container")
	_, err = cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &dockercontainer.Config{
			Hostname:     c.name,
			ExposedPorts: portSet,
			Tty:          c.TTY,
			Env:          environ,
			Cmd:          c.Args,
			Image:        image,
			User:         c.User,
			WorkingDir:   c.WorkingDir,
			Entrypoint:   c.GetCommand(),
			Labels:       map[string]string{hashLabel: expectedHash},
		},
		HostConfig: &dockercontainer.HostConfig{
			PortBindings: portBindings,
			Binds:        binds,
		},
		Platform: &v1.Platform{},
		Name:     c.name,
	})
	if ignoreConflict(err) != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}
	id, _, err = c.getContainer(ctx, cli)
	if err != nil {
		return fmt.Errorf("failed to get container ID: %w", err)
	}

	c.containerID = id
	if _, err = cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}
	go func() {
		<-ctx.Done()
		if err := c.stop(context.Background()); err != nil {
			log.Printf("failed to stop: %v", err)
		}
	}()
	logs, err := cli.ContainerLogs(ctx, c.name, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Since:      time.Now().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("failed to log container: %w", err)
	}
	defer logs.Close()
	if _, err = stdcopy.StdCopy(stdout, stderr, logs); err != nil {
		// ignore errors, might be content cancelled, we still need to wait for the container to exit
		log.Printf("failed to log container: %v", err)
	}

	waitRes := cli.ContainerWait(context.Background(), id, client.ContainerWaitOptions{Condition: dockercontainer.WaitConditionNotRunning})
	select {
	case wait := <-waitRes.Result:
		if wait.StatusCode != 0 {
			return fmt.Errorf("exit code %d", wait.StatusCode)
		}
		return nil
	case err := <-waitRes.Error:
		return fmt.Errorf("failed to wait for container: %w", err)
	}
}

func (c *container) createPorts() (network.PortSet, network.PortMap, error) {
	portSet := network.PortSet{}
	portBindings := network.PortMap{}
	for _, p := range c.Ports {
		port, err := network.ParsePort(fmt.Sprintf("%d/tcp", p.ContainerPort))
		if err != nil {
			return nil, nil, err
		}
		portSet[port] = struct{}{}
		hostPort := p.GetHostPort()
		portBindings[port] = []network.PortBinding{{
			HostPort: fmt.Sprint(hostPort),
		}}
	}
	return portSet, portBindings, nil
}

func (c *container) createBinds() ([]string, error) {
	var binds []string
	for _, mount := range c.VolumeMounts {
		for _, volume := range c.spec.Volumes {
			if volume.Name == mount.Name {
				abs, err := filepath.Abs(volume.HostPath.Path)
				if err != nil {
					return nil, err
				}
				binds = append(binds, fmt.Sprintf("%s:%s", abs, mount.MountPath))
			}
		}
	}
	return binds, nil
}

func (c *container) stop(ctx context.Context) error {
	if c.name == "" {
		return nil
	}
	log := c.log
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("failed to create docker client: %w", err)
	}
	defer cli.Close()
	id, _, err := c.getContainer(ctx, cli)
	if err != nil {
		return fmt.Errorf("failed to get container ID: %w", err)
	}
	if id == "" {
		return nil
	}
	log.Printf("stopping container\n")
	grace := c.spec.GetTerminationGracePeriod()
	timeout := int(grace.Seconds())
	_, err = cli.ContainerStop(ctx, id, client.ContainerStopOptions{
		Timeout: &timeout,
	})
	if ignoreNotExist(err) != nil {
		return fmt.Errorf("failed to stop container: %w", err)
	}
	return nil
}

const hashLabel = "kit.hash"

func (c *container) getContainer(ctx context.Context, cli *client.Client) (string, string, error) {
	list, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return "", "", err
	}
	for _, existing := range list.Items {
		if slices.Contains(existing.Names, "/"+c.name) {
			id := existing.ID
			return id, existing.Labels[hashLabel], nil
		}
	}
	return "", "", nil
}

func ignoreConflict(err error) error {
	if cerrdefs.IsConflict(err) {
		return nil
	}
	return err
}

func ignoreNotExist(err error) error {
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err

}

func (c *container) execInContainer(ctx context.Context, command []string) ([]byte, error) {
	execResp, err := c.cli.ExecCreate(ctx, c.containerID, client.ExecCreateOptions{
		Cmd:          command,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create exec instance: %w", err)
	}

	// Start exec and get response
	resp, err := c.cli.ExecAttach(ctx, execResp.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to attach to exec: %w", err)
	}
	defer resp.Close()

	// Read the output
	var stdout, stderr bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, resp.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read exec output: %w", err)
	}

	// Check exec exit code
	inspectResp, err := c.cli.ExecInspect(ctx, execResp.ID, client.ExecInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to inspect exec: %w", err)
	}

	if inspectResp.ExitCode != 0 {
		return nil, fmt.Errorf("exec command failed with exit code %d, stderr: %s", inspectResp.ExitCode, stderr.String())
	}

	return stdout.Bytes(), nil
}

var _ Interface = &container{}
