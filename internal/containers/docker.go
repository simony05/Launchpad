package containers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const internalApplicationPort = 8000
const maxCommandOutput = 64 << 10

var containerIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

// Limits configures Docker resource limits for a generated application.
type Limits struct {
	CPUs   string
	Memory string
}

// Container is a started generated application container.
type Container struct {
	ID           string
	InternalPort int
	HostPort     int
}

// Manager starts and removes generated application containers.
type Manager interface {
	Start(context.Context, string, string, int, Limits) (Container, error)
	Remove(context.Context, string) error
}

type commandRunner func(context.Context, ...string) (string, error)

// DockerManager manages generated application containers through the Docker CLI.
type DockerManager struct {
	timeout time.Duration
	run     commandRunner
}

func NewDockerManager(timeout time.Duration) *DockerManager {
	return &DockerManager{timeout: timeout, run: runDocker}
}

func (m *DockerManager) Start(ctx context.Context, deploymentID, imageName string, version int, limits Limits) (Container, error) {
	if _, err := uuid.Parse(deploymentID); err != nil {
		return Container{}, errors.New("deployment id must be a UUID")
	}
	if version < 1 || imageName == "" || limits.CPUs == "" || limits.Memory == "" {
		return Container{}, errors.New("invalid container start configuration")
	}

	startCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	containerName := fmt.Sprintf("minicloud-%s-v%d", deploymentID, version)
	output, err := m.run(startCtx,
		"run", "--detach",
		"--name", containerName,
		"--publish", "0:8000",
		"--cpus", limits.CPUs,
		"--memory", limits.Memory,
		imageName,
	)
	if err != nil {
		return Container{}, commandError(startCtx, "start container", output, err)
	}

	containerID := strings.TrimSpace(output)
	if !containerIDPattern.MatchString(containerID) {
		return Container{}, errors.New("Docker returned an invalid container ID")
	}

	portOutput, err := m.run(startCtx, "inspect", "--format", "{{.State.Running}} {{(index (index .NetworkSettings.Ports \"8000/tcp\") 0).HostPort}}", containerID)
	if err != nil {
		_ = m.remove(startCtx, containerID)
		return Container{}, commandError(startCtx, "inspect started container", portOutput, err)
	}

	fields := strings.Fields(portOutput)
	if len(fields) != 2 || fields[0] != "true" {
		_ = m.remove(startCtx, containerID)
		return Container{}, errors.New("container exited before startup completed")
	}
	hostPort, err := strconv.Atoi(fields[1])
	if err != nil || hostPort < 1 || hostPort > 65535 {
		_ = m.remove(startCtx, containerID)
		return Container{}, errors.New("Docker returned an invalid host port")
	}

	return Container{ID: containerID, InternalPort: internalApplicationPort, HostPort: hostPort}, nil
}

func (m *DockerManager) Remove(ctx context.Context, containerID string) error {
	if !containerIDPattern.MatchString(containerID) {
		return errors.New("container id is invalid")
	}
	removeCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	return m.remove(removeCtx, containerID)
}

func (m *DockerManager) remove(ctx context.Context, containerID string) error {
	output, err := m.run(ctx, "rm", "--force", containerID)
	if err != nil {
		if strings.Contains(output, "No such container") {
			return nil
		}
		return commandError(ctx, "remove container", output, err)
	}
	return nil
}

func commandError(ctx context.Context, action, output string, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s timed out", action)
	}
	message := strings.TrimSpace(output)
	if message == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %s", action, message)
}

func runDocker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	output := &limitedBuffer{limit: maxCommandOutput}
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	return output.String(), err
}

type limitedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(p) > remaining {
			b.buffer.Write(p[:remaining])
			b.truncated = true
		} else {
			b.buffer.Write(p)
		}
	} else {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return b.buffer.String() + "\n[Docker command output truncated]\n"
	}
	return b.buffer.String()
}
