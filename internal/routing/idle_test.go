package routing

import (
	"context"
	"github.com/simon/launchpad/internal/deployments"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type waitingLifecycle struct {
	ready    chan struct{}
	entered  chan struct{}
	released atomic.Bool
}

func (l *waitingLifecycle) Acquire(ctx context.Context, _ string) (func(), error) {
	close(l.entered)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.ready:
		return func() { l.released.Store(true) }, nil
	}
}
func (l *waitingLifecycle) AllowsHost(context.Context, string) bool { return true }
func TestColdStartWaitPreservesOriginalRequest(t *testing.T) {
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.EscapedPath() != "/a%2Fb" || r.URL.RawQuery != "q=x%2Fy" || string(body) != "original body" || r.Header.Get("X-Test") != "original" {
			t.Error("request changed during cold start")
		}
		w.WriteHeader(201)
	}))
	defer backend.Close()
	port := backendPort(t, backend.URL)
	router := New(&memoryResolver{deployment: deployments.Deployment{Status: deployments.StatusRunning, HostPort: &port}}, "127.0.0.1", "apps.example.com", time.Minute)
	lifecycle := &waitingLifecycle{ready: make(chan struct{}), entered: make(chan struct{})}
	router.SetLifecycle(lifecycle)
	request := httptest.NewRequest("POST", "/a%2Fb?q=x%2Fy", strings.NewReader("original body"))
	request.Host = publicIdentifier + ".apps.example.com"
	request.Header.Set("X-Test", "original")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); router.ServeHostHTTP(response, request) }()
	<-lifecycle.entered
	if hits.Load() != 0 {
		t.Fatal("forwarded before readiness")
	}
	close(lifecycle.ready)
	<-done
	if response.Code != 201 || hits.Load() != 1 || !lifecycle.released.Load() {
		t.Fatalf("status %d hits %d", response.Code, hits.Load())
	}
}
