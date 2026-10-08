package containers

import (
	"context"
	"errors"
)

// Suspend preserves the container and writable layer, unlike Remove.
func (m *DockerManager) Suspend(ctx context.Context, id string) error {
	if !containerIDPattern.MatchString(id) {
		return errors.New("invalid container id")
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	output, err := m.run(ctx, "stop", "--time", "10", id)
	if err != nil {
		return commandError(ctx, "suspend container", output, err)
	}
	return nil
}
func (m *DockerManager) Resume(ctx context.Context, id string) error {
	if !containerIDPattern.MatchString(id) {
		return errors.New("invalid container id")
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	output, err := m.run(ctx, "start", id)
	if err != nil {
		return commandError(ctx, "resume container", output, err)
	}
	return nil
}
