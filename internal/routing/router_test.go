package routing

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/simon/launchpad/internal/deployments"
)

const publicIdentifier = "aabbccddeeff00112233445566778899"

func TestRouterProxiesPathQueryBodyAndHeaders(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/nested/path" || request.URL.RawQuery != "hello=world" {
			t.Fatalf("request URL = %s", request.URL)
		}
		if request.Header.Get("X-MiniCloud-Test") != "present" {
			t.Fatal("custom header was not proxied")
		}
		body, err := io.ReadAll(request.Body)
		if err != nil || string(body) != "request body" {
			t.Fatalf("request body = %q, %v", body, err)
		}
		w.Header().Set("X-Backend", "ok")
		_, _ = w.Write([]byte("proxied"))
	}))
	defer backend.Close()

	port := backendPort(t, backend.URL)
	resolver := &memoryResolver{deployment: deployments.Deployment{Status: deployments.StatusRunning, HostPort: &port}}
	router := New(resolver, "127.0.0.1", time.Minute)
	mux := http.NewServeMux()
	mux.Handle("/apps/{publicIdentifier}", router)
	mux.Handle("/apps/{publicIdentifier}/{path...}", router)

	request := httptest.NewRequest(http.MethodPost, "/apps/"+publicIdentifier+"/nested/path?hello=world", strings.NewReader("request body"))
	request.Header.Set("X-MiniCloud-Test", "present")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "proxied" || response.Header().Get("X-Backend") != "ok" {
		t.Fatalf("response = (%d, %q, %q)", response.Code, response.Body.String(), response.Header().Get("X-Backend"))
	}
}

func TestRouterReturnsUnavailableForStoppedDeployment(t *testing.T) {
	resolver := &memoryResolver{deployment: deployments.Deployment{Status: deployments.StatusStopped}}
	router := New(resolver, "127.0.0.1", time.Minute)
	mux := http.NewServeMux()
	mux.Handle("/apps/{publicIdentifier}", router)
	request := httptest.NewRequest(http.MethodGet, "/apps/"+publicIdentifier, nil)
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

type memoryResolver struct {
	deployment deployments.Deployment
	err        error
}

func (r *memoryResolver) GetByPublicIdentifier(_ context.Context, _ string) (deployments.Deployment, error) {
	if r.err != nil {
		return deployments.Deployment{}, r.err
	}
	return r.deployment, nil
}

func backendPort(t *testing.T, backendURL string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(backendURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
