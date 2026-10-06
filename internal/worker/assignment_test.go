package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/simon/launchpad/internal/build"
	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/workspace"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type startManager struct {
	containers.Manager
	state  containers.Status
	starts int
}

func (m *startManager) Status(context.Context, string, int) (containers.Status, error) {
	return m.state, nil
}
func (m *startManager) Start(context.Context, string, string, int, containers.Limits) (containers.Container, error) {
	m.starts++
	m.state = containers.Status{Running: true, ContainerID: "ab12cd34ef56", HostPort: 32781}
	return containers.Container{ID: m.state.ContainerID, InternalPort: 8000, HostPort: 32781}, nil
}

type startBuilder struct{ calls int }

func (b *startBuilder) Build(_ context.Context, id string, v int) (build.Result, error) {
	b.calls++
	return build.Result{ImageName: build.ImageName(id, v)}, nil
}

func TestStartIdempotencyAndRevokedAssignment(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{true: "revoked after build", false: "repeated start"}[revoked], func(t *testing.T) {
			manager := &startManager{}
			builder := &startBuilder{}
			checks := 0
			guard := func(context.Context, string, int) error {
				checks++
				if revoked && checks > 1 {
					return errors.New("revoked")
				}
				return nil
			}
			server := NewServer(":0", "secret", slog.New(slog.NewTextHandler(io.Discard, nil)), workspace.NewLocalStore(t.TempDir()), builder, manager, guard)
			input := StartRequest{DeploymentID: uuid.NewString(), Version: 2, Files: workspace.Files{"app.py": "app = 1", "requirements.txt": ""}, Limits: containers.Limits{CPUs: "0.5", Memory: "256m"}}
			data, _ := json.Marshal(input)
			send := func() int {
				req := httptest.NewRequest(http.MethodPost, "/internal/deployments/start", bytes.NewReader(data))
				req.Header.Set("Authorization", "Bearer secret")
				out := httptest.NewRecorder()
				server.Handler.ServeHTTP(out, req)
				return out.Code
			}
			if revoked {
				if code := send(); code != 409 || manager.starts != 0 {
					t.Fatalf("code %d starts %d", code, manager.starts)
				}
				return
			}
			if code := send(); code != 200 {
				t.Fatal(code)
			}
			if code := send(); code != 200 || builder.calls != 1 || manager.starts != 1 {
				t.Fatalf("code %d builds %d starts %d", code, builder.calls, manager.starts)
			}
		})
	}
}

func TestAssignmentGuardFailsClosed(t *testing.T) {
	for _, code := range []int{204, 409, 503, 302} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Path != "/internal/workers/w/assignments/d" || r.URL.Query().Get("version") != "2" {
				t.Error("incorrect guard request")
			}
			w.Header().Set("Location", "http://127.0.0.1:1")
			w.WriteHeader(code)
		}))
		err := AssignmentGuard(server.URL, "w", "secret")(context.Background(), "d", 2)
		server.Close()
		if (err == nil) != (code == 204) {
			t.Fatalf("code %d err %v", code, err)
		}
	}
}
