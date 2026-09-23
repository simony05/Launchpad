package build

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/simon/launchpad/internal/workspace"
)

const testDeploymentID = "8bb34af2-396c-4b37-8905-1b93c6677a1d"

func TestBuildUsesGeneratedTemplateAndDeterministicImageName(t *testing.T) {
	root := t.TempDir()
	workspacePath := filepath.Join(root, testDeploymentID)
	if err := os.Mkdir(workspacePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, workspace.AppFilename), []byte("app = object()"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, workspace.RequirementsFilename), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	builder := NewDockerBuilder(root, time.Second)
	builder.run = func(_ context.Context, gotWorkspace, dockerfilePath, imageName string) (string, error) {
		if gotWorkspace != workspacePath {
			t.Fatalf("workspace = %q, want %q", gotWorkspace, workspacePath)
		}
		contents, err := os.ReadFile(dockerfilePath)
		if err != nil || string(contents) != pythonRuntimeDockerfile {
			t.Fatalf("runtime template = %q, %v", contents, err)
		}
		return "build complete", nil
	}

	result, err := builder.Build(context.Background(), testDeploymentID, 1)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if result.ImageName != "minicloud/deployment:8bb34af2-396c-4b37-8905-1b93c6677a1d-v1" {
		t.Fatalf("ImageName = %q", result.ImageName)
	}
	if result.Log != "build complete" {
		t.Fatalf("Log = %q", result.Log)
	}
}

func TestLimitedBufferTruncatesOutput(t *testing.T) {
	buffer := &limitedBuffer{limit: 3}
	_, _ = buffer.Write([]byte("abcd"))
	if got := buffer.String(); got != "abc\n[build output truncated]\n" {
		t.Fatalf("String() = %q", got)
	}
}
