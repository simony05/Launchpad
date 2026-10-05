package workers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memoryStore struct {
	reports []Report
	expired chan time.Duration
}

func (s *memoryStore) Record(_ context.Context, r Report) error {
	s.reports = append(s.reports, r)
	return nil
}
func (s *memoryStore) List(context.Context) ([]Worker, error) { return []Worker{}, nil }
func (s *memoryStore) Expire(_ context.Context, d time.Duration) error {
	if s.expired != nil {
		s.expired <- d
	}
	return nil
}
func validReport() Report {
	return Report{ID: "8bb34af2-396c-4b37-8905-1b93c6677a1d", Hostname: "worker-1", Address: "http://10.0.0.1:8090", Healthy: true, TotalCPU: 2, AvailableCPU: 1.5, TotalMemory: 1024, AvailableMemory: 512}
}
func TestAuthenticatedReports(t *testing.T) {
	s := &memoryStore{}
	h := Handler(s, "secret")
	for _, path := range []string{"/internal/workers/register", "/internal/workers/heartbeat"} {
		for _, token := range []string{"", "Bearer wrong", "Bearer secret"} {
			body, _ := json.Marshal(validReport())
			r := httptest.NewRequest("POST", path, strings.NewReader(string(body)))
			r.Header.Set("Authorization", token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := 401
			if token == "Bearer secret" {
				want = 204
			}
			if w.Code != want {
				t.Fatalf("%s: got %d want %d", path, w.Code, want)
			}
		}
	}
	if len(s.reports) != 2 {
		t.Fatalf("unauthorized requests reached store: %d", len(s.reports))
	}
}
func TestRejectCapacityAndTrailingJSON(t *testing.T) {
	s := &memoryStore{}
	h := Handler(s, "secret")
	report := validReport()
	report.AvailableCPU = 3
	body, _ := json.Marshal(report)
	for _, body := range []string{string(body), `{} {}`, strings.Repeat("x", 9000)} {
		r := httptest.NewRequest("POST", "/internal/workers/heartbeat", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("got %d", w.Code)
		}
	}
	if len(s.reports) != 0 {
		t.Fatal("invalid reports persisted")
	}
}
func TestMonitorRunsImmediatelyAndStops(t *testing.T) {
	s := &memoryStore{expired: make(chan time.Duration, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Monitor(ctx, s, time.Hour, 45*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case timeout := <-s.expired:
		if timeout != 45*time.Second {
			t.Fatal(timeout)
		}
	case <-time.After(time.Second):
		t.Fatal("no initial check")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("monitor did not stop")
	}
}
