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
	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/routing"
	"github.com/simon/launchpad/internal/worker"
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

// New creates the Launchpad control-plane HTTP server.
func New(address string, logger *slog.Logger, deploymentRepository deployments.Repository, workerClient worker.Client, containerLimits containers.Limits, applicationRouter routing.ApplicationRouter, publicBaseDomain string, buildTimeout, startTimeout time.Duration) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	deploymentHandler := deploymentHandler{repository: deploymentRepository, workerClient: workerClient, containerLimits: containerLimits, applicationRouter: applicationRouter, publicBaseDomain: publicBaseDomain}
	mux.HandleFunc("POST /deployments", deploymentHandler.create)
	mux.HandleFunc("GET /deployments", deploymentHandler.list)
	mux.HandleFunc("GET /deployments/{id}", deploymentHandler.get)
	mux.HandleFunc("DELETE /deployments/{id}", deploymentHandler.delete)
	mux.HandleFunc("GET /internal/tls/allow", deploymentHandler.allowTLS)
	mux.Handle("/apps/{publicIdentifier}", applicationRouter)
	mux.Handle("/apps/{publicIdentifier}/{path...}", applicationRouter)

	return &http.Server{
		Addr:              address,
		Handler:           requestLogger(logger, hostRouter(mux, applicationRouter)),
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
	repository        deployments.Repository
	workerClient      worker.Client
	containerLimits   containers.Limits
	applicationRouter routing.ApplicationRouter
	publicBaseDomain  string
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
	deployment, err = h.repository.MarkBuilding(r.Context(), deployment.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "start deployment build")
		return
	}

	result, workerErr := h.workerClient.Start(r.Context(), worker.StartRequest{DeploymentID: deployment.ID, Version: deployment.Version, Files: request.Files, Limits: h.containerLimits})
	if workerErr != nil {
		deployment, err = h.failBuild(deployment.ID, "", workerErr.Error())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "record deployment build failure")
			return
		}
		h.writeDeployment(w, http.StatusCreated, deployment)
		return
	}

	if result.BuildError != "" {
		deployment, err = h.failBuild(deployment.ID, result.BuildLog, result.BuildError)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "record deployment build failure")
			return
		}
		h.writeDeployment(w, http.StatusCreated, deployment)
		return
	}
	deployment, err = h.repository.CompleteBuild(r.Context(), deployment.ID, result.ImageName, result.BuildLog)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "record deployment build")
		return
	}
	deployment, err = h.repository.MarkStarting(r.Context(), deployment.ID)
	if err != nil {
		writeRepositoryError(w, err, "start deployment container")
		return
	}
	if result.StartError != "" || result.Container == nil {
		startError := result.StartError
		if startError == "" {
			startError = "worker did not return a started container"
		}
		deployment, err = h.failStart(deployment.ID, startError)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "record deployment startup failure")
			return
		}
		h.writeDeployment(w, http.StatusCreated, deployment)
		return
	}
	deployment, err = h.repository.CompleteStart(r.Context(), deployment.ID, result.Container.ID, result.Container.InternalPort, result.Container.HostPort)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_ = h.workerClient.Stop(cleanupCtx, result.Container.ID)
		_, _ = h.repository.FailStart(cleanupCtx, deployment.ID, "container started but Launchpad could not record its startup")
		writeRepositoryError(w, err, "record deployment startup")
		return
	}

	h.writeDeployment(w, http.StatusCreated, deployment)
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

	h.writeDeployment(w, http.StatusOK, deployment)
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
		if err := h.workerClient.Stop(r.Context(), *deployment.ContainerID); err != nil {
			writeError(w, http.StatusInternalServerError, "stop deployment container")
			return
		}
	}

	deployment, err = h.repository.MarkStopped(r.Context(), deployment.ID)
	if err != nil {
		writeRepositoryError(w, err, "stop deployment")
		return
	}
	if deployment.PublicIdentifier != nil {
		h.applicationRouter.Invalidate(*deployment.PublicIdentifier)
	}
	h.writeDeployment(w, http.StatusOK, deployment)
}

func (h deploymentHandler) list(w http.ResponseWriter, r *http.Request) {
	deploymentList, err := h.repository.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list deployments")
		return
	}

	for index := range deploymentList {
		h.decorateDeployment(&deploymentList[index])
	}
	writeJSON(w, http.StatusOK, deploymentList)
}

func (h deploymentHandler) allowTLS(w http.ResponseWriter, r *http.Request) {
	if !h.applicationRouter.AllowsHost(r.Context(), r.URL.Query().Get("domain")) {
		writeError(w, http.StatusForbidden, "certificate not authorized")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h deploymentHandler) writeDeployment(w http.ResponseWriter, status int, deployment deployments.Deployment) {
	h.decorateDeployment(&deployment)
	writeJSON(w, status, deployment)
}

func (h deploymentHandler) decorateDeployment(deployment *deployments.Deployment) {
	if deployment.Status != deployments.StatusRunning || deployment.PublicIdentifier == nil {
		return
	}
	publicURL := "https://" + *deployment.PublicIdentifier + "." + h.publicBaseDomain
	deployment.PublicURL = &publicURL
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

func hostRouter(next http.Handler, applicationRouter routing.ApplicationRouter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if applicationRouter.MatchesHost(r.Host) {
			applicationRouter.ServeHostHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
