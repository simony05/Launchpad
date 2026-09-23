package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/simon/launchpad/internal/workspace"
)

const maxBuildLogSize = 256 << 10

const pythonRuntimeDockerfile = `FROM python:3.13-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

WORKDIR /app

COPY requirements.txt ./
RUN python -m pip install --no-cache-dir -r requirements.txt

COPY app.py ./

EXPOSE 8000

CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
`

// Result contains the observable result of a Docker image build.
type Result struct {
	ImageName string
	Log       string
}

// Builder creates an image from an existing deployment workspace.
type Builder interface {
	Build(context.Context, string, int) (Result, error)
}

type dockerRunner func(context.Context, string, string, string) (string, error)

// DockerBuilder invokes Docker using the MiniCloud-owned Python runtime template.
type DockerBuilder struct {
	workspaceRoot string
	timeout       time.Duration
	run           dockerRunner
}

func NewDockerBuilder(workspaceRoot string, timeout time.Duration) *DockerBuilder {
	return &DockerBuilder{
		workspaceRoot: workspaceRoot,
		timeout:       timeout,
		run:           runDockerBuild,
	}
}

func (b *DockerBuilder) Build(ctx context.Context, deploymentID string, version int) (Result, error) {
	if _, err := uuid.Parse(deploymentID); err != nil {
		return Result{}, errors.New("deployment id must be a UUID")
	}
	if version < 1 {
		return Result{}, errors.New("deployment version must be positive")
	}

	workspacePath, err := workspace.Path(b.workspaceRoot, deploymentID)
	if err != nil {
		return Result{}, err
	}
	dockerfilePath, err := writeRuntimeTemplate(workspacePath)
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(dockerfilePath)

	imageName := ImageName(deploymentID, version)
	buildCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	log, err := b.run(buildCtx, workspacePath, dockerfilePath, imageName)
	result := Result{ImageName: imageName, Log: log}
	if err == nil {
		return result, nil
	}
	if errors.Is(buildCtx.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("build timed out after %s", b.timeout)
	}
	return result, fmt.Errorf("docker build failed: %w", err)
}

// ImageName is deterministic and contains no caller-provided name data.
func ImageName(deploymentID string, version int) string {
	return fmt.Sprintf("minicloud/deployment:%s-v%d", deploymentID, version)
}

func writeRuntimeTemplate(workspacePath string) (string, error) {
	info, err := os.Stat(workspacePath)
	if err != nil {
		return "", fmt.Errorf("inspect deployment workspace: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("deployment workspace is not a directory")
	}

	file, err := os.CreateTemp(workspacePath, ".minicloud-Dockerfile-")
	if err != nil {
		return "", fmt.Errorf("create runtime template: %w", err)
	}
	path := file.Name()
	if _, err := file.WriteString(pythonRuntimeDockerfile); err != nil {
		file.Close()
		os.Remove(path)
		return "", fmt.Errorf("write runtime template: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close runtime template: %w", err)
	}
	return filepath.Clean(path), nil
}

func runDockerBuild(ctx context.Context, workspacePath, dockerfilePath, imageName string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "build", "--progress=plain", "--pull=false", "--file", dockerfilePath, "--tag", imageName, workspacePath)
	output := &limitedBuffer{limit: maxBuildLogSize}
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
		return b.buffer.String() + "\n[build output truncated]\n"
	}
	return b.buffer.String()
}
