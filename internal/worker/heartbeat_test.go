package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/workers"
)

type heartbeatSource struct{ fail bool }

func (s heartbeatSource) Resources(context.Context) (containers.Resources, error) {
	if s.fail {
		return containers.Resources{}, errors.New("Docker down")
	}
	return containers.Resources{CPUs: 2, MemoryBytes: 1024, AvailableCPU: 1, AvailableMemory: 512, ContainersRunning: 1}, nil
}
func TestHeartbeatRegistrationRetryAndRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths := make(chan string, 4)
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing auth")
		}
		var report workers.Report
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Error(err)
		}
		if !report.Healthy || report.AvailableCPU != 1 || report.AvailableMemory != 512 {
			t.Error("incorrect capacity report")
		}
		paths <- r.URL.Path
		count++
		if count == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
		if count == 3 {
			cancel()
		}
	}))
	defer server.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ReportHeartbeats(ctx, server.URL, "secret", workers.Report{ID: "test"}, heartbeatSource{}, 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reporter failed to stop")
	}
	for _, want := range []string{"/internal/workers/register", "/internal/workers/register", "/internal/workers/heartbeat"} {
		if got := <-paths; got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	}
}
func TestDockerFailureReportsUnhealthy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var report workers.Report
		_ = json.NewDecoder(r.Body).Decode(&report)
		if report.Healthy {
			t.Error("Docker failure reported healthy")
		}
		w.WriteHeader(204)
		cancel()
	}))
	defer server.Close()
	ReportHeartbeats(ctx, server.URL, "secret", workers.Report{}, heartbeatSource{fail: true}, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
}
