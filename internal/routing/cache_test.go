package routing

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simon/launchpad/internal/deployments"
)

type countingResolver struct {
	calls      atomic.Int32
	deployment deployments.Deployment
	err        error
}

func (r *countingResolver) GetByPublicIdentifier(context.Context, string) (deployments.Deployment, error) {
	r.calls.Add(1)
	return r.deployment, r.err
}

func TestCacheCoalescesAndExpires(t *testing.T) {
	port := 32781
	resolver := &countingResolver{deployment: deployments.Deployment{Status: deployments.StatusRunning, HostPort: &port}}
	router := New(resolver, "10.0.0.1", "apps.example.com", 5*time.Second)
	now := time.Now()
	router.now = func() time.Time { return now }
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := router.resolve(context.Background(), publicIdentifier); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if resolver.calls.Load() != 1 {
		t.Fatalf("cache stampede: %d", resolver.calls.Load())
	}
	now = now.Add(6 * time.Second)
	_, _ = router.resolve(context.Background(), publicIdentifier)
	if resolver.calls.Load() != 2 {
		t.Fatal("cache did not expire")
	}
	router.Invalidate(publicIdentifier)
	resolver.deployment.Status = deployments.StatusStopped
	if _, err := router.resolve(context.Background(), publicIdentifier); err == nil {
		t.Fatal("stopped deployment cached as running")
	}
}

func TestNegativeCacheAndBound(t *testing.T) {
	resolver := &countingResolver{err: deployments.ErrNotFound}
	router := New(resolver, "10.0.0.1", "apps.example.com", 5*time.Second)
	now := time.Now()
	router.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		_, _ = router.resolve(context.Background(), publicIdentifier)
	}
	if resolver.calls.Load() != 1 {
		t.Fatal("negative result not cached")
	}
	now = now.Add(2 * time.Second)
	_, _ = router.resolve(context.Background(), publicIdentifier)
	if resolver.calls.Load() != 2 {
		t.Fatal("negative cache did not expire")
	}
	for i := 0; i < 1100; i++ {
		_, _ = router.resolve(context.Background(), fmt.Sprintf("%032x", i))
	}
	if len(router.cache) > 1024 {
		t.Fatal("unbounded cache")
	}
}

func TestProxyFailureEvictsAndDoesNotReplayPost(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	resolver := &countingResolver{deployment: deployments.Deployment{Status: deployments.StatusRunning, HostPort: &port}}
	router := New(resolver, "127.0.0.1", "apps.example.com", time.Minute)
	request := httptest.NewRequest("POST", "/", nil)
	request.Host = publicIdentifier + ".apps.example.com"
	response := httptest.NewRecorder()
	router.ServeHostHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("got %d", response.Code)
	}
	if _, ok := router.cache[publicIdentifier]; ok {
		t.Fatal("failed route retained")
	}
	if resolver.calls.Load() != 1 {
		t.Fatal("request retried")
	}
}
