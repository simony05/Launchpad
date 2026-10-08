package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/simon/launchpad/internal/containers"
)

var lifecycleLocks [128]sync.Mutex

func lifecycleLock(id string) *sync.Mutex {
	var h uint32
	for _, b := range []byte(id) {
		h = h*31 + uint32(b)
	}
	return &lifecycleLocks[h%128]
}

type IdleRequest struct {
	DeploymentID string `json:"deployment_id"`
	Version      int    `json:"version"`
	Epoch        int    `json:"epoch"`
	ContainerID  string `json:"container_id"`
	Action       string `json:"action"`
}
type IdleClient interface {
	Idle(context.Context, IdleRequest) (Status, error)
}
type IdleManager interface {
	Status(context.Context, string, int) (containers.Status, error)
	Suspend(context.Context, string) error
	Resume(context.Context, string) error
}

func (c *HTTPClient) Idle(ctx context.Context, input IdleRequest) (Status, error) {
	var result Status
	err := c.doJSON(ctx, http.MethodPost, "/internal/deployments/idle", input, &result)
	return result, err
}

// EnableIdle wraps the existing authenticated API without changing its endpoints.
func EnableIdle(server *http.Server, token, origin, workerID string, manager IdleManager, workspaceRemovers ...interface {
	Remove(context.Context, string) error
}) {
	old := server.Handler
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cleanupEndpoint := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			DeploymentID string `json:"deployment_id"`
			Version      int    `json:"version"`
		}
		if err := decodeJSON(w, r, &input); err != nil {
			writeError(w, 400, "invalid cleanup request")
			return
		}
		if _, err := uuid.Parse(input.DeploymentID); err != nil || input.Version < 1 {
			writeError(w, 400, "invalid cleanup identity")
			return
		}
		lock := lifecycleLock(input.DeploymentID)
		lock.Lock()
		defer lock.Unlock()
		address := fmt.Sprintf("%s/internal/workers/%s/cleanup/%s?version=%d", strings.TrimRight(origin, "/"), workerID, input.DeploymentID, input.Version)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, address, nil)
		if err != nil {
			writeError(w, 503, "cleanup authorization failed")
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(req)
		if err != nil {
			writeError(w, 503, "cleanup authorization unavailable")
			return
		}
		response.Body.Close()
		if response.StatusCode != 204 {
			writeError(w, 409, "cleanup authorization revoked")
			return
		}
		cleaner, ok := manager.(interface {
			CleanupDeployment(context.Context, string, int) error
		})
		if !ok {
			writeError(w, 501, "worker cleanup is unavailable")
			return
		}
		if err := cleaner.CleanupDeployment(r.Context(), input.DeploymentID, input.Version); err != nil {
			writeError(w, 503, "remove deployment container and image")
			return
		}
		for _, remover := range workspaceRemovers {
			if err := remover.Remove(r.Context(), input.DeploymentID); err != nil {
				writeError(w, 503, "remove deployment workspace")
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	endpoint := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input IdleRequest
		if err := decodeJSON(w, r, &input); err != nil {
			writeError(w, 400, "invalid idle request")
			return
		}
		if _, err := uuid.Parse(input.DeploymentID); err != nil || input.Version < 1 || input.Epoch < 1 || (input.Action != "sleep" && input.Action != "wake") {
			writeError(w, 400, "invalid idle operation")
			return
		}
		lock := lifecycleLock(input.DeploymentID)
		lock.Lock()
		defer lock.Unlock()
		address := fmt.Sprintf("%s/internal/workers/%s/idle/%s?version=%d&epoch=%d&action=%s", strings.TrimRight(origin, "/"), workerID, input.DeploymentID, input.Version, input.Epoch, input.Action)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, address, nil)
		if err != nil {
			writeError(w, 503, "assignment check failed")
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(req)
		if err != nil {
			writeError(w, 503, "assignment check unavailable")
			return
		}
		response.Body.Close()
		if response.StatusCode != 204 {
			writeError(w, 409, "idle operation superseded")
			return
		}
		state, err := manager.Status(r.Context(), input.DeploymentID, input.Version)
		if err != nil || state.ContainerID != input.ContainerID || state.ContainerID == "" {
			writeError(w, 409, "container identity unavailable")
			return
		}
		if input.Action == "sleep" && state.Running {
			err = manager.Suspend(r.Context(), input.ContainerID)
		}
		if input.Action == "wake" && !state.Running {
			err = manager.Resume(r.Context(), input.ContainerID)
		}
		if err == nil {
			state, err = manager.Status(r.Context(), input.DeploymentID, input.Version)
		}
		if err == nil && (state.Running != (input.Action == "wake")) {
			err = errors.New("container state not confirmed")
		}
		if err != nil {
			writeError(w, 503, "idle operation failed")
			return
		}
		writeJSON(w, 200, Status{DeploymentID: input.DeploymentID, Version: input.Version, Running: state.Running, ContainerID: state.ContainerID, HostPort: state.HostPort})
	})
	authenticated := requireToken(token, endpoint)
	authenticatedCleanup := requireToken(token, cleanupEndpoint)
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/deployments/idle" && r.Method == http.MethodPost {
			authenticated.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/internal/deployments/cleanup" && r.Method == http.MethodPost {
			authenticatedCleanup.ServeHTTP(w, r)
			return
		}
		old.ServeHTTP(w, r)
	})
}
