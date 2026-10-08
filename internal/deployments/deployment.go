package deployments

import (
	"time"

	"github.com/simon/launchpad/internal/workspace"
)

// Status describes a deployment's lifecycle state.
type Status string

const (
	StatusPending      Status = "PENDING"
	StatusBuilding     Status = "BUILDING"
	StatusStarting     Status = "STARTING"
	StatusRunning      Status = "RUNNING"
	StatusFailed       Status = "FAILED"
	StatusStopped      Status = "STOPPED"
	StatusReadyToStart Status = "READY_TO_START"
	StatusRecovering   Status = "RECOVERING"
	StatusSuspending   Status = "SUSPENDING"
	StatusSleeping     Status = "SLEEPING"
	StatusWaking       Status = "WAKING"
	StatusStopping     Status = "STOPPING"
)

// Deployment is Launchpad's metadata record for an application deployment.
type Deployment struct {
	LastRequestAt    time.Time  `json:"last_request_at"`
	IdleEpoch        int        `json:"-"`
	IdleError        *string    `json:"idle_error"`
	ColdStartMS      *int64     `json:"cold_start_ms"`
	FailoverAttempts int        `json:"failover_attempts"`
	RecoveryError    *string    `json:"recovery_error"`
	RuntimeLogs      string     `json:"runtime_logs"`
	HealthPath       string     `json:"health_path"`
	HealthState      *string    `json:"health_state"`
	HealthCheckedAt  *time.Time `json:"health_checked_at"`
	RestartAttempts  int        `json:"restart_attempts"`
	NextRestartAt    *time.Time `json:"next_restart_at"`
	LastExitCode     *int       `json:"last_exit_code"`
	OOMKilled        bool       `json:"oom_killed"`
	RuntimeError     *string    `json:"runtime_error"`
	LastFailure      *string    `json:"last_failure"`
	WorkerID         *string    `json:"worker_id"`
	WorkerAddress    *string    `json:"worker_address"`
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Status           Status     `json:"status"`
	Runtime          string     `json:"runtime"`
	Version          int        `json:"version"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	ContainerID      *string    `json:"container_id"`
	InternalPort     *int       `json:"internal_port"`
	HostPort         *int       `json:"host_port"`
	PublicIdentifier *string    `json:"public_identifier"`
	PublicURL        *string    `json:"public_url"`
	ImageName        *string    `json:"image_name"`
	BuildLog         *string    `json:"build_log"`
	BuildError       *string    `json:"build_error"`
	StartError       *string    `json:"start_error"`
}

// CreateInput contains metadata accepted when creating a deployment.
type CreateInput struct {
	Files      workspace.Files
	HealthPath string
	Name       string
	Runtime    string
}
