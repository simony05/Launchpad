package deployments

import (
	"context"
	"errors"
)

var ErrNotFound = errors.New("deployment not found")

// Repository provides persistence for deployment metadata.
type Repository interface {
	Create(context.Context, CreateInput) (Deployment, error)
	Get(context.Context, string) (Deployment, error)
	List(context.Context) ([]Deployment, error)
	Delete(context.Context, string) error
}
