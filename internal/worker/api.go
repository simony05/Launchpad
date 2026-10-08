package worker

import (
	"context"

	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/workspace"
)

// StartRequest contains the source and immutable metadata required to build
// and start one deployment on a worker.
type StartRequest struct {
	DeploymentID string            `json:"deployment_id"`
	Version      int               `json:"version"`
	Files        workspace.Files   `json:"files"`
	Limits       containers.Limits `json:"limits"`
}

// StartResult reports both build output and the started container location.
type StartResult struct {
	ImageName  string                `json:"image_name,omitempty"`
	BuildLog   string                `json:"build_log,omitempty"`
	BuildError string                `json:"build_error,omitempty"`
	StartError string                `json:"start_error,omitempty"`
	Container  *containers.Container `json:"container,omitempty"`
}

// Status reports the worker's view of a deployment container.
type Status struct {
	DeploymentID string `json:"deployment_id"`
	Version      int    `json:"version"`
	Running      bool   `json:"running"`
	ContainerID  string `json:"container_id,omitempty"`
	HostPort     int    `json:"host_port,omitempty"`
}

// Resources reports basic host capacity and Docker workload counts.
type Resources struct {
	CPUs              int   `json:"cpus"`
	MemoryBytes       int64 `json:"memory_bytes"`
	ContainersRunning int   `json:"containers_running"`
}

// Client is the control plane's worker boundary. It deliberately contains no
// Docker-specific implementation details.
type Client interface {
	Start(context.Context, StartRequest) (StartResult, error)
	Stop(context.Context, string) error
	Status(context.Context, string, int) (Status, error)
	Resources(context.Context) (Resources, error)
}

type CleanupClient interface {
	Cleanup(context.Context, string, int) error
}
