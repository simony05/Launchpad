package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const (
	AppFilename          = "app.py"
	RequirementsFilename = "requirements.txt"
	maxAppSize           = 1 << 20
	maxRequirementsSize  = 64 << 10
)

// Files is the V1 source bundle accepted for a Python deployment.
type Files map[string]string

// Store persists source files for a deployment.
type Store interface {
	Store(context.Context, string, Files) error
}

// LocalStore persists deployment source under a configured local root.
type LocalStore struct {
	root string
}

func NewLocalStore(root string) *LocalStore {
	return &LocalStore{root: root}
}

// Path returns the canonical workspace path for a deployment UUID.
func Path(root, deploymentID string) (string, error) {
	if _, err := uuid.Parse(deploymentID); err != nil {
		return "", errors.New("deployment workspace id must be a UUID")
	}
	return filepath.Join(root, deploymentID), nil
}

// ValidateFiles permits exactly the V1 source files and enforces size limits.
func ValidateFiles(files Files) error {
	if len(files) != 2 {
		return errors.New("files must contain exactly app.py and requirements.txt")
	}

	app, ok := files[AppFilename]
	if !ok || strings.TrimSpace(app) == "" {
		return errors.New("files.app.py must be non-empty")
	}
	if len(app) > maxAppSize {
		return fmt.Errorf("files.app.py must not exceed %d bytes", maxAppSize)
	}

	requirements, ok := files[RequirementsFilename]
	if !ok {
		return errors.New("files.requirements.txt is required")
	}
	if len(requirements) > maxRequirementsSize {
		return fmt.Errorf("files.requirements.txt must not exceed %d bytes", maxRequirementsSize)
	}

	for name := range files {
		if name != AppFilename && name != RequirementsFilename {
			return fmt.Errorf("unsupported filename %q", name)
		}
	}

	return nil
}

// Store writes a complete source bundle through a temporary directory before
// atomically making it visible as the deployment workspace.
func (s *LocalStore) Store(ctx context.Context, deploymentID string, files Files) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := Path(s.root, deploymentID); err != nil {
		return err
	}
	if err := ValidateFiles(files); err != nil {
		return err
	}
	if err := os.MkdirAll(s.root, 0o750); err != nil {
		return fmt.Errorf("create workspace root: %w", err)
	}

	finalPath, _ := Path(s.root, deploymentID)
	if _, err := os.Lstat(finalPath); err == nil {
		return errors.New("deployment workspace already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect deployment workspace: %w", err)
	}

	temporaryPath, err := os.MkdirTemp(s.root, ".workspace-")
	if err != nil {
		return fmt.Errorf("create temporary workspace: %w", err)
	}
	defer os.RemoveAll(temporaryPath)

	if err := writeFile(temporaryPath, AppFilename, files[AppFilename]); err != nil {
		return err
	}
	if err := writeFile(temporaryPath, RequirementsFilename, files[RequirementsFilename]); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return fmt.Errorf("publish deployment workspace: %w", err)
	}

	return nil
}

func writeFile(directory, filename, contents string) error {
	if err := os.WriteFile(filepath.Join(directory, filename), []byte(contents), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", filename, err)
	}
	return nil
}
