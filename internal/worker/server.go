package worker

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/simon/launchpad/internal/build"
	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/workspace"
)

const maxRequestBodySize = 2 << 20

// NewServer exposes the worker's authenticated, private API.
func NewServer(address, token string, logger *slog.Logger, sourceStore workspace.Store, builder build.Builder, manager containers.Manager, guards ...StartGuard) *http.Server {
	handler := handler{sourceStore: sourceStore, builder: builder, manager: manager}
	if len(guards) > 0 {
		handler.guard = guards[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handler.health)
	mux.HandleFunc("POST /internal/deployments/start", handler.start)
	mux.HandleFunc("POST /internal/deployments/stop", handler.stop)
	mux.HandleFunc("GET /internal/deployments/{id}/status", handler.status)
	mux.HandleFunc("GET /internal/resources", handler.resources)
	return &http.Server{
		Addr:              address,
		Handler:           requestLogger(logger, requireToken(token, mux)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      6 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
}

type handler struct {
	locks       [64]sync.Mutex
	guard       StartGuard
	sourceStore workspace.Store
	builder     build.Builder
	manager     containers.Manager
}

func (h *handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) start(w http.ResponseWriter, request *http.Request) {
	var input StartRequest
	if err := decodeJSON(w, request, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := uuid.Parse(input.DeploymentID); err != nil || input.Version < 1 || input.Limits.CPUs == "" || input.Limits.Memory == "" {
		writeError(w, http.StatusBadRequest, "invalid deployment start request")
		return
	}
	if err := workspace.ValidateFiles(input.Files); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Bounded lock striping serializes retries without retaining a lock per app.
	var stripe uint32
	for _, b := range []byte(input.DeploymentID) {
		stripe = stripe*31 + uint32(b)
	}
	h.locks[stripe%64].Lock()
	defer h.locks[stripe%64].Unlock()
	allowed := func() bool {
		if h.guard != nil {
			if err := h.guard(request.Context(), input.DeploymentID, input.Version); err != nil {
				writeError(w, http.StatusConflict, "deployment assignment is unavailable or superseded")
				return false
			}
		}
		return true
	}
	if !allowed() {
		return
	}
	if h.guard != nil {
		status, err := h.manager.Status(request.Context(), input.DeploymentID, input.Version)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "inspect existing deployment before start")
			return
		}
		if status.ContainerID != "" {
			if !status.Running {
				writeJSON(w, http.StatusOK, StartResult{StartError: "existing container is stopped; start was not repeated"})
				return
			}
			writeJSON(w, http.StatusOK, StartResult{ImageName: build.ImageName(input.DeploymentID, input.Version), Container: &containers.Container{ID: status.ContainerID, InternalPort: 8000, HostPort: status.HostPort}})
			return
		}
	}
	if err := h.sourceStore.Store(request.Context(), input.DeploymentID, input.Files); err != nil {
		writeError(w, http.StatusInternalServerError, "store deployment source")
		return
	}
	buildResult, buildErr := h.builder.Build(request.Context(), input.DeploymentID, input.Version)
	result := StartResult{ImageName: buildResult.ImageName, BuildLog: buildResult.Log}
	if buildErr != nil {
		result.BuildError = buildErr.Error()
		writeJSON(w, http.StatusOK, result)
		return
	}
	if !allowed() {
		return
	}
	container, startErr := h.manager.Start(request.Context(), input.DeploymentID, buildResult.ImageName, input.Version, input.Limits)
	if startErr != nil {
		result.StartError = startErr.Error()
		writeJSON(w, http.StatusOK, result)
		return
	}
	result.Container = &container
	writeJSON(w, http.StatusOK, result)
}

func (h *handler) stop(w http.ResponseWriter, request *http.Request) {
	var input struct {
		ContainerID string `json:"container_id"`
	}
	if err := decodeJSON(w, request, &input); err != nil || input.ContainerID == "" {
		writeError(w, http.StatusBadRequest, "container_id is required")
		return
	}
	if err := h.manager.Remove(request.Context(), input.ContainerID); err != nil {
		writeError(w, http.StatusInternalServerError, "stop deployment container")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) status(w http.ResponseWriter, request *http.Request) {
	deploymentID := request.PathValue("id")
	version, err := strconv.Atoi(request.URL.Query().Get("version"))
	if _, parseErr := uuid.Parse(deploymentID); parseErr != nil || err != nil || version < 1 {
		writeError(w, http.StatusBadRequest, "deployment id and positive version are required")
		return
	}
	status, err := h.manager.Status(request.Context(), deploymentID, version)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get deployment status")
		return
	}
	writeJSON(w, http.StatusOK, Status{DeploymentID: deploymentID, Version: version, Running: status.Running, ContainerID: status.ContainerID, HostPort: status.HostPort})
}

func (h *handler) resources(w http.ResponseWriter, request *http.Request) {
	resources, err := h.manager.Resources(request.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get worker resources")
		return
	}
	writeJSON(w, http.StatusOK, Resources{CPUs: resources.CPUs, MemoryBytes: resources.MemoryBytes, ContainersRunning: resources.ContainersRunning})
}

func requireToken(token string, next http.Handler) http.Handler {
	expected := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/health" {
			next.ServeHTTP(w, request)
			return
		}
		provided := []byte(request.Header.Get("Authorization"))
		if len(provided) != len(expected) || subtle.ConstantTimeCompare(provided, expected) != 1 {
			writeError(w, http.StatusUnauthorized, "worker authentication required")
			return
		}
		next.ServeHTTP(w, request)
	})
}

func decodeJSON(w http.ResponseWriter, request *http.Request, output any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, maxRequestBodySize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return errors.New("request body must be valid JSON")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, request)
		logger.Info("worker request", "method", request.Method, "path", request.URL.Path, "duration", time.Since(started))
	})
}
