package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	if body := response.Body.String(); !strings.Contains(body, `"status":"PENDING"`) {
		t.Fatalf("body = %q, want PENDING deployment", body)
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
	return New("", slog.New(slog.NewTextHandler(io.Discard, nil)), &memoryRepository{}, &memorySourceStore{})
}

type memoryRepository struct {
	items []deployments.Deployment
}

func (r *memoryRepository) Create(_ context.Context, input deployments.CreateInput) (deployments.Deployment, error) {
	deployment := deployments.Deployment{
		ID:        "8bb34af2-396c-4b37-8905-1b93c6677a1d",
		Name:      input.Name,
		Runtime:   input.Runtime,
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

type memorySourceStore struct {
	files map[string]workspace.Files
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
