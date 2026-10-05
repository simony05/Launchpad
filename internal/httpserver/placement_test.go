package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/scheduler"
	"github.com/simon/launchpad/internal/worker"
)

type testScheduler struct {
	err      error
	released bool
	repo     *memoryRepository
}

func (s *testScheduler) Reserve(_ context.Context, id string, _ containers.Limits) (scheduler.Assignment, error) {
	if s.err != nil {
		return scheduler.Assignment{}, s.err
	}
	a := scheduler.Assignment{WorkerID: "worker-b", Address: "http://10.0.0.2:8090"}
	for i := range s.repo.items {
		if s.repo.items[i].ID == id {
			s.repo.items[i].WorkerID = &a.WorkerID
			s.repo.items[i].WorkerAddress = &a.Address
		}
	}
	return a, nil
}
func (s *testScheduler) Release(context.Context, string) error { s.released = true; return nil }

func TestPlacementCreatesAndStopsOnSelectedWorker(t *testing.T) {
	repo := &memoryRepository{}
	s := &testScheduler{repo: repo}
	chosen := &memoryWorker{}
	legacy := &memoryWorker{}
	var addresses []string
	placement := Placement{Scheduler: s, Client: func(address string) (worker.Client, error) {
		addresses = append(addresses, address)
		return chosen, nil
	}}
	server := New("", slog.New(slog.NewTextHandler(io.Discard, nil)), repo, legacy, containers.Limits{CPUs: "0.5", Memory: "256m"}, newTestRouter(repo), "apps.example.com", time.Minute, time.Second, placement)
	request := httptest.NewRequest("POST", "/deployments", strings.NewReader(`{"name":"test","runtime":"python","files":{"app.py":"app = object()","requirements.txt":""}}`))
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, request)
	if response.Code != 201 || !strings.Contains(response.Body.String(), `"worker_id":"worker-b"`) {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	id := repo.items[0].ID
	response = httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest("DELETE", "/deployments/"+id, nil))
	if response.Code != 200 || !s.released || chosen.removedID == "" || legacy.removedID != "" {
		t.Fatalf("wrong stop behavior: %d", response.Code)
	}
	if len(addresses) != 2 || addresses[0] != "http://10.0.0.2:8090" || addresses[1] != addresses[0] {
		t.Fatal(addresses)
	}
}

func TestNoCapacityDoesNotCallWorker(t *testing.T) {
	repo := &memoryRepository{}
	s := &testScheduler{repo: repo, err: scheduler.ErrNoCapacity}
	placement := Placement{Scheduler: s, Client: func(string) (worker.Client, error) {
		t.Error("worker called with no capacity")
		return nil, errors.New("unexpected")
	}}
	server := New("", slog.New(slog.NewTextHandler(io.Discard, nil)), repo, nil, containers.Limits{CPUs: "0.5", Memory: "256m"}, newTestRouter(repo), "apps.example.com", time.Minute, time.Second, placement)
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest("POST", "/deployments", strings.NewReader(`{"name":"test","runtime":"python","files":{"app.py":"app = object()","requirements.txt":""}}`)))
	if response.Code != 503 || repo.items[0].Status != deployments.StatusFailed {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
}

type errorWorker struct {
	memoryWorker
	result worker.StartResult
	err    error
}

func (w *errorWorker) Start(context.Context, worker.StartRequest) (worker.StartResult, error) {
	return w.result, w.err
}

func TestReservationReleaseDependsOnConfirmedOutcome(t *testing.T) {
	for _, test := range []struct {
		name    string
		result  worker.StartResult
		err     error
		release bool
	}{
		{"build failure", worker.StartResult{BuildError: "pip failed"}, nil, true},
		{"uncertain network result", worker.StartResult{}, errors.New("connection lost"), false},
		{"uncertain startup", worker.StartResult{ImageName: "image", StartError: "timeout"}, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &memoryRepository{}
			s := &testScheduler{repo: repo}
			placement := Placement{Scheduler: s, Client: func(string) (worker.Client, error) { return &errorWorker{result: test.result, err: test.err}, nil }}
			server := New("", slog.New(slog.NewTextHandler(io.Discard, nil)), repo, nil, containers.Limits{CPUs: "0.5", Memory: "256m"}, newTestRouter(repo), "apps.example.com", time.Minute, time.Second, placement)
			response := httptest.NewRecorder()
			server.Handler.ServeHTTP(response, httptest.NewRequest("POST", "/deployments", strings.NewReader(`{"name":"test","runtime":"python","files":{"app.py":"app = object()","requirements.txt":""}}`)))
			if response.Code != 201 || repo.items[0].Status != deployments.StatusFailed || s.released != test.release {
				t.Fatalf("status %d released %v body %s", response.Code, s.released, response.Body.String())
			}
		})
	}
}
