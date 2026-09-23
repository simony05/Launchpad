package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/simon/launchpad/internal/build"
	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/workspace"
)

const (
	readHeaderTimeout  = 5 * time.Second
	readTimeout        = 15 * time.Second
	idleTimeout        = 60 * time.Second
	maxRequestBodySize = 2 << 20
	cleanupTimeout     = 5 * time.Second
	buildResponseGrace = 15 * time.Second
)

// New creates the MiniCloud control-plane HTTP server.
func New(address string, logger *slog.Logger, deploymentRepository deployments.Repository, sourceStore workspace.Store, builder build.Builder, containerManager containers.Manager, containerLimits containers.Limits, buildTimeout, startTimeout time.Duration) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	deploymentHandler := deploymentHandler{repository: deploymentRepository, sourceStore: sourceStore, builder: builder, containerManager: containerManager, containerLimits: containerLimits}
	mux.HandleFunc("POST /deployments", deploymentHandler.create)
	mux.HandleFunc("GET /deployments", deploymentHandler.list)
	mux.HandleFunc("GET /deployments/{id}", deploymentHandler.get)
	mux.HandleFunc("DELETE /deployments/{id}", deploymentHandler.delete)

	return &http.Server{
		Addr:              address,
		Handler:           requestLogger(logger, mux),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      buildTimeout + startTimeout + buildResponseGrace,
		IdleTimeout:       idleTimeout,
	}
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

type deploymentHandler struct {
	repository       deployments.Repository
	sourceStore      workspace.Store
	builder          build.Builder
	containerManager containers.Manager
	containerLimits  containers.Limits
}

type createDeploymentRequest struct {
	Name    string          `json:"name"`
	Runtime string          `json:"runtime"`
	Files   workspace.Files `json:"files"`
}

func (h deploymentHandler) create(w http.ResponseWriter, r *http.Request) {
	request, err := decodeCreateDeploymentRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	deployment, err := h.repository.Create(r.Context(), deployments.CreateInput{
		Name:    request.Name,
		Runtime: request.Runtime,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create deployment")
		return
	}
	if err := h.sourceStore.Store(r.Context(), deployment.ID, request.Files); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if cleanupErr := h.repository.Delete(cleanupCtx, deployment.ID); cleanupErr != nil {
			writeError(w, http.StatusInternalServerError, "create deployment")
			return
		}
		writeError(w, http.StatusInternalServerError, "store deployment source")
		return
	}

	deployment, err = h.repository.MarkBuilding(r.Context(), deployment.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "start deployment build")
		return
	}

	buildResult, buildErr := h.builder.Build(r.Context(), deployment.ID, deployment.Version)
	if buildErr != nil {
		deployment, err = h.failBuild(deployment.ID, buildResult.Log, buildErr.Error())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "record deployment build failure")
			return
		}
		writeJSON(w, http.StatusCreated, deployment)
		return
	}

	deployment, err = h.repository.CompleteBuild(r.Context(), deployment.ID, buildResult.ImageName, buildResult.Log)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "record deployment build")
		return
	}
	deployment, err = h.repository.MarkStarting(r.Context(), deployment.ID)
	if err != nil {
		writeRepositoryError(w, err, "start deployment container")
		return
	}
	if deployment.ImageName == nil {
		writeError(w, http.StatusInternalServerError, "deployment image is missing")
		return
	}
	container, startErr := h.containerManager.Start(r.Context(), deployment.ID, *deployment.ImageName, deployment.Version, h.containerLimits)
	if startErr != nil {
		deployment, err = h.failStart(deployment.ID, startErr.Error())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "record deployment startup failure")
			return
		}
		writeJSON(w, http.StatusCreated, deployment)
		return
	}
	deployment, err = h.repository.CompleteStart(r.Context(), deployment.ID, container.ID, container.InternalPort, container.HostPort)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_ = h.containerManager.Remove(cleanupCtx, container.ID)
		_, _ = h.repository.FailStart(cleanupCtx, deployment.ID, "container started but MiniCloud could not record its startup")
		writeRepositoryError(w, err, "record deployment startup")
		return
	}

	writeJSON(w, http.StatusCreated, deployment)
}

func (h deploymentHandler) failBuild(id, buildLog, buildError string) (deployments.Deployment, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	return h.repository.FailBuild(ctx, id, buildLog, buildError)
}

func (h deploymentHandler) failStart(id, startError string) (deployments.Deployment, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	return h.repository.FailStart(ctx, id, startError)
}

func (h deploymentHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "deployment id must be a UUID")
		return
	}

	deployment, err := h.repository.Get(r.Context(), id)
	if errors.Is(err, deployments.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deployment not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get deployment")
		return
	}

	writeJSON(w, http.StatusOK, deployment)
}

func (h deploymentHandler) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "deployment id must be a UUID")
		return
	}

	deployment, err := h.repository.Get(r.Context(), id)
	if errors.Is(err, deployments.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deployment not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get deployment")
		return
	}
	if deployment.ContainerID != nil {
		if err := h.containerManager.Remove(r.Context(), *deployment.ContainerID); err != nil {
			writeError(w, http.StatusInternalServerError, "stop deployment container")
			return
		}
	}

	deployment, err = h.repository.MarkStopped(r.Context(), deployment.ID)
	if err != nil {
		writeRepositoryError(w, err, "stop deployment")
		return
	}
	writeJSON(w, http.StatusOK, deployment)
}

func (h deploymentHandler) list(w http.ResponseWriter, r *http.Request) {
	deploymentList, err := h.repository.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list deployments")
		return
	}

	writeJSON(w, http.StatusOK, deploymentList)
}

func decodeCreateDeploymentRequest(w http.ResponseWriter, r *http.Request) (createDeploymentRequest, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodySize))
	decoder.DisallowUnknownFields()

	var request createDeploymentRequest
	if err := decoder.Decode(&request); err != nil {
		return createDeploymentRequest{}, errors.New("request body must be valid JSON with name, runtime, and files")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return createDeploymentRequest{}, errors.New("request body must contain one JSON object")
	}

	request.Name = strings.TrimSpace(request.Name)
	request.Runtime = strings.TrimSpace(request.Runtime)
	if request.Name == "" || len(request.Name) > 100 {
		return createDeploymentRequest{}, errors.New("name must contain 1 to 100 characters")
	}
	if request.Runtime != "python" {
		return createDeploymentRequest{}, errors.New("runtime must be python")
	}
	if err := workspace.ValidateFiles(request.Files); err != nil {
		return createDeploymentRequest{}, err
	}

	return request, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{Error: message})
}

func writeRepositoryError(w http.ResponseWriter, err error, message string) {
	if errors.Is(err, deployments.ErrInvalidState) {
		writeError(w, http.StatusConflict, "deployment is busy")
		return
	}
	writeError(w, http.StatusInternalServerError, message)
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"duration", time.Since(started),
		)
	})
}
