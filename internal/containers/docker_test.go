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

	container, err := manager.Start(context.Background(), testDeploymentID, "launchpad/deployment:test-v1", 1, Limits{CPUs: "0.5", Memory: "256m"})
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

func TestResourceAvailability(t *testing.T) {
	for _, test := range []struct {
		name, limits string
		cpu          float64
		memory       int64
	}{
		{"limited", `{"NanoCpus":500000000,"Memory":256}`, 1.5, 768},
		{"quota", `{"CpuQuota":100000,"CpuPeriod":100000,"Memory":512}`, 1, 512},
		{"unlimited", `{}`, 0, 0},
		{"overcommitted", `{"NanoCpus":4000000000,"Memory":2048}`, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := NewDockerManager(time.Second)
			m.run = func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "info":
					return "2 1024 1", nil
				case "ps":
					return testContainerID, nil
				case "inspect":
					return test.limits, nil
				}
				return "", fmt.Errorf("unexpected command")
			}
			r, err := m.Resources(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if r.AvailableCPU != test.cpu || r.AvailableMemory != test.memory || r.ContainersRunning != 1 {
				t.Fatalf("unexpected resources: %+v", r)
			}
		})
	}
}
