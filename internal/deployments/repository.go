package deployments

import (
	"context"
	"errors"
)

var ErrNotFound = errors.New("deployment not found")
var ErrInvalidState = errors.New("deployment is not in a valid state for this operation")

// Repository provides persistence for deployment metadata.
type Repository interface {
	Create(context.Context, CreateInput) (Deployment, error)
	Get(context.Context, string) (Deployment, error)
	GetByPublicIdentifier(context.Context, string) (Deployment, error)
	List(context.Context) ([]Deployment, error)
	Delete(context.Context, string) error
	MarkBuilding(context.Context, string) (Deployment, error)
	CompleteBuild(context.Context, string, string, string) (Deployment, error)
	FailBuild(context.Context, string, string, string) (Deployment, error)
	MarkStarting(context.Context, string) (Deployment, error)
	CompleteStart(context.Context, string, string, int, int) (Deployment, error)
	FailStart(context.Context, string, string) (Deployment, error)
	MarkStopped(context.Context, string) (Deployment, error)
}
