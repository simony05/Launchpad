package deployments

import "time"

// Status describes a deployment's lifecycle state.
type Status string

const (
	StatusPending  Status = "PENDING"
	StatusBuilding Status = "BUILDING"
	StatusStarting Status = "STARTING"
	StatusRunning  Status = "RUNNING"
	StatusFailed   Status = "FAILED"
	StatusStopped  Status = "STOPPED"
)

// Deployment is MiniCloud's metadata record for an application deployment.
type Deployment struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Status           Status    `json:"status"`
	Runtime          string    `json:"runtime"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	ContainerID      *string   `json:"container_id"`
	InternalPort     *int      `json:"internal_port"`
	PublicIdentifier *string   `json:"public_identifier"`
}

// CreateInput contains metadata accepted when creating a deployment.
type CreateInput struct {
	Name    string
	Runtime string
}
