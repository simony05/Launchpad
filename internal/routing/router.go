package routing

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/simon/launchpad/internal/deployments"
)

var publicIdentifierPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Resolver provides deployment locations for the application router.
type Resolver interface {
	GetByPublicIdentifier(context.Context, string) (deployments.Deployment, error)
}

// ApplicationRouter is an HTTP router whose cache can be invalidated when a
// deployment stops.
type ApplicationRouter interface {
	http.Handler
	Invalidate(string)
}

type cacheEntry struct {
	hostPort int
	expires  time.Time
}

// Router resolves a public application identifier and proxies it to the
// deployment's published Docker host port.
type Router struct {
	resolver     Resolver
	upstreamHost string
	cacheTTL     time.Duration

	mu    sync.RWMutex
	cache map[string]cacheEntry
	now   func() time.Time
}

func New(resolver Resolver, upstreamHost string, cacheTTL time.Duration) *Router {
	return &Router{
		resolver:     resolver,
		upstreamHost: upstreamHost,
		cacheTTL:     cacheTTL,
		cache:        make(map[string]cacheEntry),
		now:          time.Now,
	}
}

func (r *Router) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	publicIdentifier := request.PathValue("publicIdentifier")
	if !publicIdentifierPattern.MatchString(publicIdentifier) {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}

	hostPort, err := r.resolve(request.Context(), publicIdentifier)
	if errors.Is(err, deployments.ErrNotFound) {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "application is unavailable")
		return
	}

	target := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(r.upstreamHost, strconv.Itoa(hostPort)),
	}
	path, rawPath := applicationPath(request, publicIdentifier)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			proxyRequest.SetURL(target)
			proxyRequest.Out.URL.Path = path
			proxyRequest.Out.URL.RawPath = rawPath
			proxyRequest.SetXForwarded()
		},
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, _ error) {
			writeError(response, http.StatusBadGateway, "application upstream is unavailable")
		},
	}
	proxy.ServeHTTP(w, request)
}

// Invalidate removes a location cache entry after a deployment changes state.
func (r *Router) Invalidate(publicIdentifier string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, publicIdentifier)
}

func (r *Router) resolve(ctx context.Context, publicIdentifier string) (int, error) {
	now := r.now()
	r.mu.RLock()
	entry, ok := r.cache[publicIdentifier]
	r.mu.RUnlock()
	if ok && now.Before(entry.expires) {
		return entry.hostPort, nil
	}

	deployment, err := r.resolver.GetByPublicIdentifier(ctx, publicIdentifier)
	if err != nil {
		return 0, err
	}
	if deployment.Status != deployments.StatusRunning || deployment.HostPort == nil {
		return 0, errors.New("deployment is not running")
	}

	r.mu.Lock()
	r.cache[publicIdentifier] = cacheEntry{hostPort: *deployment.HostPort, expires: now.Add(r.cacheTTL)}
	r.mu.Unlock()
	return *deployment.HostPort, nil
}

func applicationPath(request *http.Request, publicIdentifier string) (string, string) {
	prefix := "/apps/" + publicIdentifier
	path := strings.TrimPrefix(request.URL.Path, prefix)
	rawPath := strings.TrimPrefix(request.URL.EscapedPath(), prefix)
	if path == "" {
		return "/", ""
	}
	if rawPath == path {
		rawPath = ""
	}
	return path, rawPath
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: message})
}
