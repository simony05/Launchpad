package containers

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

func (m *DockerManager) Restart(ctx context.Context, id string) error {
	if !containerIDPattern.MatchString(id) {
		return errors.New("invalid container ID")
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	// Only start an exited container; Docker restart would interrupt a live one.
	output, err := m.run(ctx, "start", id)
	if err != nil {
		return commandError(ctx, "restart exited container", output, err)
	}
	return nil
}

func (m *DockerManager) LogTail(ctx context.Context, id string) (string, error) {
	if !containerIDPattern.MatchString(id) {
		return "", errors.New("invalid container ID")
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	output, err := m.run(ctx, "logs", "--timestamps", "--tail", "50", id)
	if err != nil {
		return "", commandError(ctx, "read container logs", output, err)
	}
	if len(output) > 4096 {
		output = output[:4096]
	}
	output = strings.ReplaceAll(strings.ToValidUTF8(output, "?"), "\x00", "")
	if len(output) > 4096 {
		output = output[:4096]
	}
	for !utf8.ValidString(output) {
		output = output[:len(output)-1]
	}
	return output, nil
}
