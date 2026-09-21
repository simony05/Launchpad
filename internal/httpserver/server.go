package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/simon/launchpad/internal/deployments"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
)

// New creates the MiniCloud control-plane HTTP server.
func New(address string, logger *slog.Logger, deploymentRepository deployments.Repository) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	deploymentHandler := deploymentHandler{repository: deploymentRepository}
	mux.HandleFunc("POST /deployments", deploymentHandler.create)
	mux.HandleFunc("GET /deployments", deploymentHandler.list)
	mux.HandleFunc("GET /deployments/{id}", deploymentHandler.get)

	return &http.Server{
		Addr:              address,
		Handler:           requestLogger(logger, mux),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

type deploymentHandler struct {
	repository deployments.Repository
}

type createDeploymentRequest struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
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

	writeJSON(w, http.StatusCreated, deployment)
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

func (h deploymentHandler) list(w http.ResponseWriter, r *http.Request) {
	deploymentList, err := h.repository.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list deployments")
		return
	}

	writeJSON(w, http.StatusOK, deploymentList)
}

func decodeCreateDeploymentRequest(w http.ResponseWriter, r *http.Request) (createDeploymentRequest, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()

	var request createDeploymentRequest
	if err := decoder.Decode(&request); err != nil {
		return createDeploymentRequest{}, errors.New("request body must be valid JSON with name and runtime")
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
