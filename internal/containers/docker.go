package containers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

// Status is the observed state of a generated application container.
type Status struct {
	Running     bool
	ContainerID string
	HostPort    int
}

// Resources is the subset of Docker host capacity needed by the first worker.
type Resources struct {
	AvailableCPU      float64
	AvailableMemory   int64
	CPUs              int
	MemoryBytes       int64
	ContainersRunning int
}

// Manager starts and removes generated application containers.
type Manager interface {
	Start(context.Context, string, string, int, Limits) (Container, error)
	Remove(context.Context, string) error
	Status(context.Context, string, int) (Status, error)
	Resources(context.Context) (Resources, error)
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
	containerName := fmt.Sprintf("launchpad-%s-v%d", deploymentID, version)
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

func (m *DockerManager) Status(ctx context.Context, deploymentID string, version int) (Status, error) {
	if _, err := uuid.Parse(deploymentID); err != nil || version < 1 {
		return Status{}, errors.New("invalid deployment status configuration")
	}
	statusCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	containerName := fmt.Sprintf("launchpad-%s-v%d", deploymentID, version)
	output, err := m.run(statusCtx, "inspect", "--format", "{{.State.Running}} {{.Id}} {{(index (index .NetworkSettings.Ports \"8000/tcp\") 0).HostPort}}", containerName)
	if err != nil {
		if strings.Contains(output, "No such object") {
			return Status{}, nil
		}
		return Status{}, commandError(statusCtx, "inspect container", output, err)
	}
	fields := strings.Fields(output)
	if len(fields) != 3 {
		return Status{}, errors.New("Docker returned an invalid container status")
	}
	hostPort, err := strconv.Atoi(fields[2])
	if err != nil || hostPort < 1 || hostPort > 65535 {
		return Status{}, errors.New("Docker returned an invalid host port")
	}
	return Status{Running: fields[0] == "true", ContainerID: fields[1], HostPort: hostPort}, nil
}

func (m *DockerManager) Resources(ctx context.Context) (Resources, error) {
	resourcesCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	output, err := m.run(resourcesCtx, "info", "--format", "{{.NCPU}} {{.MemTotal}} {{.ContainersRunning}}")
	if err != nil {
		return Resources{}, commandError(resourcesCtx, "inspect Docker resources", output, err)
	}
	fields := strings.Fields(output)
	if len(fields) != 3 {
		return Resources{}, errors.New("Docker returned invalid resource information")
	}
	cpus, cpuErr := strconv.Atoi(fields[0])
	memoryBytes, memoryErr := strconv.ParseInt(fields[1], 10, 64)
	running, runningErr := strconv.Atoi(fields[2])
	if cpuErr != nil || memoryErr != nil || runningErr != nil || cpus < 1 || memoryBytes < 1 || running < 0 {
		return Resources{}, errors.New("Docker returned invalid resource information")
	}
	ids, err := m.run(resourcesCtx, "ps", "--quiet", "--no-trunc")
	if err != nil {
		return Resources{}, commandError(resourcesCtx, "list running containers", ids, err)
	}
	availableCPU, availableMemory := float64(cpus), memoryBytes
	containerIDs := strings.Fields(ids)
	for _, id := range containerIDs {
		if !containerIDPattern.MatchString(id) {
			return Resources{}, errors.New("invalid container ID in resource sample")
		}
		output, err := m.run(resourcesCtx, "inspect", "--format", "{{json .HostConfig}}", id)
		if err != nil {
			return Resources{}, commandError(resourcesCtx, "inspect resource limits", output, err)
		}
		var limits struct {
			NanoCpus  int64
			CpuQuota  int64
			CpuPeriod int64
			Memory    int64
		}
		if err := json.Unmarshal([]byte(output), &limits); err != nil {
			return Resources{}, err
		}
		cpu := float64(limits.NanoCpus) / 1e9
		if cpu == 0 && limits.CpuQuota > 0 && limits.CpuPeriod > 0 {
			cpu = float64(limits.CpuQuota) / float64(limits.CpuPeriod)
		}
		// An unlimited container can consume the whole host; do not advertise it as free.
		if cpu <= 0 {
			availableCPU = 0
		} else {
			availableCPU -= cpu
		}
		if limits.Memory <= 0 {
			availableMemory = 0
		} else {
			availableMemory -= limits.Memory
		}
	}
	return Resources{CPUs: cpus, MemoryBytes: memoryBytes, ContainersRunning: len(containerIDs), AvailableCPU: math.Max(0, availableCPU), AvailableMemory: max(0, availableMemory)}, nil
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
