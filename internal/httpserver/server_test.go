package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simon/launchpad/internal/build"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/workspace"
)

func TestHealth(t *testing.T) {
	server := newTestServer()
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	response := httptest.NewRecorder()

	server.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	if body := response.Body.String(); body != "{\"status\":\"ok\"}\n" {
		t.Fatalf("body = %q, want health response", body)
	}
}

func TestHealthRejectsOtherMethods(t *testing.T) {
	server := newTestServer()
	request := httptest.NewRequest(http.MethodPost, "/health", nil)
	response := httptest.NewRecorder()

	server.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestCreateDeployment(t *testing.T) {
	server := newTestServer()
	request := httptest.NewRequest(http.MethodPost, "/deployments", strings.NewReader(`{"name":"demo-api","runtime":"python","files":{"app.py":"from fastapi import FastAPI","requirements.txt":"fastapi"}}`))
	response := httptest.NewRecorder()

	server.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusCreated)
	}
	if body := response.Body.String(); !strings.Contains(body, `"status":"READY_TO_START"`) {
		t.Fatalf("body = %q, want built deployment", body)
	}
}

func TestCreateDeploymentRejectsUnexpectedFilename(t *testing.T) {
	server := newTestServer()
	request := httptest.NewRequest(http.MethodPost, "/deployments", strings.NewReader(`{"name":"demo-api","runtime":"python","files":{"app.py":"app = object()","requirements.txt":"","../secret":"no"}}`))
	response := httptest.NewRecorder()

	server.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestCreateDeploymentRecordsBuildFailure(t *testing.T) {
	server := newTestServerWithBuilder(failingBuilder{})
	request := httptest.NewRequest(http.MethodPost, "/deployments", strings.NewReader(`{"name":"broken-api","runtime":"python","files":{"app.py":"from fastapi import FastAPI","requirements.txt":"not-a-real-package"}}`))
	response := httptest.NewRecorder()

	server.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusCreated)
	}
	if body := response.Body.String(); !strings.Contains(body, `"status":"FAILED"`) || !strings.Contains(body, `"build_error":"docker build failed"`) {
		t.Fatalf("body = %q, want persisted build failure", body)
	}
}

func TestGetDeploymentReturnsNotFound(t *testing.T) {
	server := newTestServer()
	request := httptest.NewRequest(http.MethodGet, "/deployments/8bb34af2-396c-4b37-8905-1b93c6677a1d", nil)
	response := httptest.NewRecorder()

	server.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestListDeployments(t *testing.T) {
	server := newTestServer()
	request := httptest.NewRequest(http.MethodGet, "/deployments", nil)
	response := httptest.NewRecorder()

	server.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "[]\n" {
		t.Fatalf("response = (%d, %q), want empty deployment list", response.Code, response.Body.String())
	}
}

func newTestServer() *http.Server {
	return newTestServerWithBuilder(successfulBuilder{})
}

func newTestServerWithBuilder(builder build.Builder) *http.Server {
	return New("", slog.New(slog.NewTextHandler(io.Discard, nil)), &memoryRepository{}, &memorySourceStore{}, builder, time.Minute)
}

type memoryRepository struct {
	items []deployments.Deployment
}

func (r *memoryRepository) Create(_ context.Context, input deployments.CreateInput) (deployments.Deployment, error) {
	deployment := deployments.Deployment{
		ID:        "8bb34af2-396c-4b37-8905-1b93c6677a1d",
		Name:      input.Name,
		Runtime:   input.Runtime,
		Version:   1,
		Status:    deployments.StatusPending,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	r.items = append(r.items, deployment)
	return deployment, nil
}

func (r *memoryRepository) Get(_ context.Context, id string) (deployments.Deployment, error) {
	for _, deployment := range r.items {
		if deployment.ID == id {
			return deployment, nil
		}
	}
	return deployments.Deployment{}, deployments.ErrNotFound
}

func (r *memoryRepository) List(_ context.Context) ([]deployments.Deployment, error) {
	if r.items == nil {
		return []deployments.Deployment{}, nil
	}
	return r.items, nil
}

func (r *memoryRepository) Delete(_ context.Context, id string) error {
	for index, deployment := range r.items {
		if deployment.ID == id {
			r.items = append(r.items[:index], r.items[index+1:]...)
			return nil
		}
	}
	return deployments.ErrNotFound
}

func (r *memoryRepository) MarkBuilding(_ context.Context, id string) (deployments.Deployment, error) {
	for index, deployment := range r.items {
		if deployment.ID == id && deployment.Status == deployments.StatusPending {
			deployment.Status = deployments.StatusBuilding
			r.items[index] = deployment
			return deployment, nil
		}
	}
	return deployments.Deployment{}, deployments.ErrNotFound
}

func (r *memoryRepository) CompleteBuild(_ context.Context, id, imageName, buildLog string) (deployments.Deployment, error) {
	for index, deployment := range r.items {
		if deployment.ID == id && deployment.Status == deployments.StatusBuilding {
			deployment.Status = deployments.StatusReadyToStart
			deployment.ImageName = &imageName
			deployment.BuildLog = &buildLog
			r.items[index] = deployment
			return deployment, nil
		}
	}
	return deployments.Deployment{}, deployments.ErrNotFound
}

func (r *memoryRepository) FailBuild(_ context.Context, id, buildLog, buildError string) (deployments.Deployment, error) {
	for index, deployment := range r.items {
		if deployment.ID == id && deployment.Status == deployments.StatusBuilding {
			deployment.Status = deployments.StatusFailed
			deployment.BuildLog = &buildLog
			deployment.BuildError = &buildError
			r.items[index] = deployment
			return deployment, nil
		}
	}
	return deployments.Deployment{}, deployments.ErrNotFound
}

type memorySourceStore struct {
	files map[string]workspace.Files
}

type successfulBuilder struct{}

func (successfulBuilder) Build(_ context.Context, deploymentID string, version int) (build.Result, error) {
	return build.Result{ImageName: build.ImageName(deploymentID, version), Log: "build complete"}, nil
}

type failingBuilder struct{}

func (failingBuilder) Build(_ context.Context, deploymentID string, version int) (build.Result, error) {
	return build.Result{ImageName: build.ImageName(deploymentID, version), Log: "pip install failed"}, errors.New("docker build failed")
}

func (s *memorySourceStore) Store(_ context.Context, deploymentID string, files workspace.Files) error {
	if s.files == nil {
		s.files = make(map[string]workspace.Files)
	}
	s.files[deploymentID] = files
	return nil
}

var _ deployments.Repository = (*memoryRepository)(nil)
var _ workspace.Store = (*memorySourceStore)(nil)
var _ build.Builder = successfulBuilder{}
var _ build.Builder = failingBuilder{}
