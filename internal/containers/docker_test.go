package containers

import (
	"context"
	"fmt"
	"testing"
	"time"
)

const testDeploymentID = "8bb34af2-396c-4b37-8905-1b93c6677a1d"
const testContainerID = "ab12cd34ef56"

func TestStartRecordsDockerAssignedPort(t *testing.T) {
	manager := NewDockerManager(time.Second)
	var calls [][]string
	manager.run = func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args)
		switch args[0] {
		case "run":
			return testContainerID + "\n", nil
		case "inspect":
			return "true 32781\n", nil
		default:
			return "", fmt.Errorf("unexpected command %q", args[0])
		}
	}

	container, err := manager.Start(context.Background(), testDeploymentID, "minicloud/deployment:test-v1", 1, Limits{CPUs: "0.5", Memory: "256m"})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if container.ID != testContainerID || container.InternalPort != 8000 || container.HostPort != 32781 {
		t.Fatalf("Container = %#v", container)
	}
	if len(calls) != 2 || calls[0][0] != "run" || calls[1][0] != "inspect" {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestRemoveTreatsMissingContainerAsStopped(t *testing.T) {
	manager := NewDockerManager(time.Second)
	manager.run = func(_ context.Context, args ...string) (string, error) {
		return "Error response from daemon: No such container: " + args[len(args)-1], fmt.Errorf("exit status 1")
	}

	if err := manager.Remove(context.Background(), testContainerID); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
}
