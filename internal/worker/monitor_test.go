package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/workers"
)

type recoveryFake struct {
	state                 containers.Status
	statusErr, restartErr error
	restarts              int
}

func (f *recoveryFake) LogTail(context.Context, string) (string, error) {
	return "Traceback: test crash", nil
}

func (f *recoveryFake) Status(context.Context, string, int) (containers.Status, error) {
	return f.state, f.statusErr
}
func (f *recoveryFake) Restart(context.Context, string) error { f.restarts++; return f.restartErr }

func TestMonitorRecoveryRequiresRecordedClaim(t *testing.T) {
	for _, test := range []struct {
		name     string
		claim    bool
		httpCode int
		want     int
	}{{"authorized", true, 200, 1}, {"budget exhausted", false, 200, 0}, {"registry down", true, 503, 0}} {
		t.Run(test.name, func(t *testing.T) {
			cid := "ab12cd34ef56"
			f := &recoveryFake{state: containers.Status{ContainerID: cid, ExitCode: 137, OOMKilled: true}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer secret" {
					t.Error("missing auth")
				}
				var o workers.Observation
				if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
					t.Error(err)
				}
				if o.State != "EXITED" || !o.RequestRestart || o.ExitCode != 137 || !o.OOMKilled {
					t.Errorf("bad report %+v", o)
				}
				if f.restarts != 0 {
					t.Error("restart occurred before recording claim")
				}
				w.WriteHeader(test.httpCode)
				_ = json.NewEncoder(w).Encode(workers.RecoveryDecision{Restart: test.claim})
			}))
			defer server.Close()
			monitor := ApplicationMonitor{Manager: f, RegistryURL: server.URL, WorkerID: "worker", Token: "secret", Client: server.Client(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			_ = monitor.check(context.Background(), deployments.Deployment{ID: "deployment", Version: 1, ContainerID: &cid})
			if f.restarts != test.want {
				t.Fatalf("restarts %d", f.restarts)
			}
		})
	}
}

func TestMonitorDockerOutageDoesNotReportCrash(t *testing.T) {
	f := &recoveryFake{statusErr: errors.New("Docker socket unavailable")}
	cid := "ab12cd34ef56"
	m := ApplicationMonitor{Manager: f}
	if err := m.check(context.Background(), deployments.Deployment{ContainerID: &cid}); err == nil || f.restarts != 0 {
		t.Fatal("outage treated as recoverable crash")
	}
}

func TestHTTPHealthCheckDoesNotLeakTokenOrFollowRedirect(t *testing.T) {
	for _, code := range []int{200, 204, 302, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("worker token leaked")
				}
				if r.URL.Path != "/health" {
					t.Error("wrong path")
				}
				w.Header().Set("Location", "http://127.0.0.1:1/private")
				w.WriteHeader(code)
			}))
			defer app.Close()
			u, _ := url.Parse(app.URL)
			port, _ := strconv.Atoi(u.Port())
			m := ApplicationMonitor{AppHost: u.Hostname(), Token: "secret"}
			err := m.probe(context.Background(), port, "/health")
			if (err == nil) != (code >= 200 && code < 300) {
				t.Fatalf("status %d error %v", code, err)
			}
		})
	}
}
