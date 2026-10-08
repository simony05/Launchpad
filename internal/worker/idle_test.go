package worker

import (
	"context"
	"github.com/google/uuid"
	"github.com/simon/launchpad/internal/containers"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type idleManagerFake struct {
	running       bool
	starts, stops int
}

func (m *idleManagerFake) Status(context.Context, string, int) (containers.Status, error) {
	return containers.Status{Running: m.running, ContainerID: "abcdef123456", HostPort: 32781}, nil
}
func (m *idleManagerFake) Suspend(context.Context, string) error {
	m.stops++
	m.running = false
	return nil
}
func (m *idleManagerFake) Resume(context.Context, string) error {
	m.starts++
	m.running = true
	return nil
}
func TestIdleAuthorizationAndIdempotency(t *testing.T) {
	var authorized atomic.Bool
	authorized.Store(true)
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Query().Get("epoch") != "3" {
			t.Error("missing lifecycle identity")
		}
		if !authorized.Load() {
			w.WriteHeader(409)
			return
		}
		w.WriteHeader(204)
	}))
	defer registry.Close()
	manager := &idleManagerFake{}
	api := &http.Server{Handler: http.NotFoundHandler()}
	EnableIdle(api, "secret", registry.URL, uuid.NewString(), manager)
	server := httptest.NewServer(api.Handler)
	defer server.Close()
	client, err := NewHTTPClient(server.URL, "secret", 0)
	if err != nil {
		t.Fatal(err)
	}
	input := IdleRequest{DeploymentID: uuid.NewString(), Version: 1, Epoch: 3, ContainerID: "abcdef123456", Action: "wake"}
	for i := 0; i < 2; i++ {
		if _, err := client.Idle(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	if manager.starts != 1 {
		t.Fatal("duplicate start")
	}
	input.Action = "sleep"
	for i := 0; i < 2; i++ {
		if _, err := client.Idle(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	if manager.stops != 1 {
		t.Fatal("duplicate stop")
	}
	authorized.Store(false)
	input.Action = "wake"
	if _, err := client.Idle(context.Background(), input); err == nil || manager.starts != 1 {
		t.Fatal("stale operation executed")
	}
}
