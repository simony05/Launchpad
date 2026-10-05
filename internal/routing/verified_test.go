package routing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/worker"
	"github.com/simon/launchpad/internal/workers"
)

type testWorkerLookup struct {
	value workers.Worker
	err   error
}

func (w testWorkerLookup) Get(context.Context, string) (workers.Worker, error) { return w.value, w.err }

type statusClient struct {
	worker.Client
	status worker.Status
	err    error
}

func (c statusClient) Status(context.Context, string, int) (worker.Status, error) {
	return c.status, c.err
}

func TestVerifiedRoutes(t *testing.T) {
	for _, name := range []string{"healthy", "stopped", "stale", "unhealthy", "worker missing", "worker unreachable", "replaced container", "changed address", "public address", "missing container", "invalid port"} {
		t.Run(name, func(t *testing.T) {
			id, cid, wid, address := "deployment", "container", "worker", "http://10.0.0.2:8090"
			port := 32781
			d := deployments.Deployment{ID: id, Version: 1, Status: deployments.StatusRunning, ContainerID: &cid, WorkerID: &wid, WorkerAddress: &address, HostPort: &port}
			lookup := testWorkerLookup{value: workers.Worker{Report: workers.Report{Address: address}, Status: "HEALTHY", LastHeartbeat: time.Now()}}
			client := statusClient{status: worker.Status{DeploymentID: id, Version: 1, ContainerID: cid, Running: true, HostPort: 32782}}
			switch name {
			case "stopped":
				client.status.Running = false
			case "stale":
				lookup.value.LastHeartbeat = time.Now().Add(-time.Minute)
			case "unhealthy":
				lookup.value.Status = "UNHEALTHY"
			case "worker missing":
				lookup.err = errors.New("missing")
			case "worker unreachable":
				client.err = errors.New("timeout")
			case "replaced container":
				client.status.ContainerID = "other"
			case "changed address":
				lookup.value.Address = "http://10.0.0.3:8090"
			case "public address":
				address = "http://8.8.8.8:8090"
				lookup.value.Address = address
			case "missing container":
				d.ContainerID = nil
			case "invalid port":
				client.status.HostPort = 0
			}
			v := VerifiedResolver{Deployments: &memoryResolver{deployment: d}, Workers: lookup, HeartbeatTimeout: 45 * time.Second, Client: func(string) (worker.Client, error) { return client, nil }}
			got, err := v.GetByPublicIdentifier(context.Background(), publicIdentifier)
			if name == "healthy" {
				if err != nil || *got.HostPort != 32782 {
					t.Fatalf("did not use current port: %+v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("unsafe route accepted")
			}
		})
	}
}
