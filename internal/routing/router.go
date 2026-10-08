package routing

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
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

type Lifecycle interface {
	Acquire(context.Context, string) (func(), error)
	AllowsHost(context.Context, string) bool
}

// ApplicationRouter is an HTTP router whose cache can be invalidated when a
// deployment stops.
type ApplicationRouter interface {
	http.Handler
	ServeHostHTTP(http.ResponseWriter, *http.Request)
	MatchesHost(string) bool
	AllowsHost(context.Context, string) bool
	Invalidate(string)
}

type cacheEntry struct {
	err                 error
	host                string
	hostPort            int
	expires             time.Time
	deploymentExpiresAt *time.Time
}

// Router resolves a public application identifier and proxies it to the
// deployment's published Docker host port.
type Router struct {
	lifecycle        Lifecycle
	resolver         Resolver
	upstreamHost     string
	publicBaseDomain string
	cacheTTL         time.Duration

	mu        sync.RWMutex
	cache     map[string]cacheEntry
	now       func() time.Time
	lookups   [64]sync.Mutex
	transport *http.Transport
}

// SetLifecycle must be called before the server accepts requests.
func (r *Router) SetLifecycle(l Lifecycle) { r.lifecycle = l }

func New(resolver Resolver, upstreamHost, publicBaseDomain string, cacheTTL time.Duration) *Router {
	return &Router{
		resolver:         resolver,
		upstreamHost:     upstreamHost,
		publicBaseDomain: strings.ToLower(strings.TrimSuffix(publicBaseDomain, ".")),
		cacheTTL:         cacheTTL,
		cache:            make(map[string]cacheEntry),
		now:              time.Now,
		transport:        &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ResponseHeaderTimeout: 10 * time.Second, IdleConnTimeout: 60 * time.Second, MaxIdleConns: 100, MaxIdleConnsPerHost: 10},
	}
}

func (r *Router) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	publicIdentifier := request.PathValue("publicIdentifier")
	path, rawPath := applicationPath(request, publicIdentifier)
	r.proxy(w, request, publicIdentifier, path, rawPath)
}

// ServeHostHTTP proxies a request whose subdomain identifies an application.
func (r *Router) ServeHostHTTP(w http.ResponseWriter, request *http.Request) {
	publicIdentifier, ok := r.publicIdentifierFromHost(request.Host)
	if !ok {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}
	r.proxy(w, request, publicIdentifier, request.URL.Path, request.URL.RawPath)
}

// MatchesHost reports whether a Host header is a valid Launchpad app hostname.
func (r *Router) MatchesHost(host string) bool {
	_, ok := r.publicIdentifierFromHost(host)
	return ok
}

// AllowsHost is used by the TLS edge before issuing an on-demand certificate.
func (r *Router) AllowsHost(ctx context.Context, host string) bool {
	publicIdentifier, ok := r.publicIdentifierFromHost(host)
	if !ok {
		return false
	}
	if r.lifecycle != nil {
		return r.lifecycle.AllowsHost(ctx, publicIdentifier)
	}
	_, err := r.resolve(ctx, publicIdentifier)
	return err == nil
}

func (r *Router) proxy(w http.ResponseWriter, request *http.Request, publicIdentifier, path, rawPath string) {
	if !publicIdentifierPattern.MatchString(publicIdentifier) {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}

	if r.lifecycle != nil {
		release, err := r.lifecycle.Acquire(request.Context(), publicIdentifier)
		if err != nil {
			code := http.StatusServiceUnavailable
			if errors.Is(err, deployments.ErrNotFound) {
				code = http.StatusNotFound
			} else if errors.Is(err, deployments.ErrExpired) {
				code = http.StatusGone
			}
			w.Header().Set("Retry-After", "2")
			writeError(w, code, "application is unavailable")
			return
		}
		defer release()
	}
	location, err := r.resolve(request.Context(), publicIdentifier)
	if errors.Is(err, deployments.ErrNotFound) {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}
	if errors.Is(err, deployments.ErrExpired) {
		writeError(w, http.StatusGone, "application has expired")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "application is unavailable")
		return
	}

	target := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(location.host, strconv.Itoa(location.hostPort)),
	}
	proxy := &httputil.ReverseProxy{
		Transport: r.transport,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			proxyRequest.SetURL(target)
			proxyRequest.Out.URL.Path = path
			proxyRequest.Out.URL.RawPath = rawPath
			proxyRequest.SetXForwarded()
		},
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, _ error) {
			r.Invalidate(publicIdentifier)
			writeError(response, http.StatusBadGateway, "application upstream is unavailable")
		},
	}
	proxy.ServeHTTP(w, request)
}

// Invalidate removes a location cache entry after a deployment changes state.
func (r *Router) Invalidate(publicIdentifier string) {
	lock := r.lookupLock(publicIdentifier)
	lock.Lock()
	defer lock.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, publicIdentifier)
}

func (r *Router) lookupLock(id string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return &r.lookups[h.Sum32()%uint32(len(r.lookups))]
}

func (r *Router) resolve(ctx context.Context, publicIdentifier string) (cacheEntry, error) {
	// Striped locks coalesce concurrent misses and serialize invalidation with
	// in-flight lookups without an unbounded per-identifier lock map.
	lock := r.lookupLock(publicIdentifier)
	lock.Lock()
	defer lock.Unlock()
	now := r.now()
	r.mu.RLock()
	entry, ok := r.cache[publicIdentifier]
	r.mu.RUnlock()
	if ok && now.Before(entry.expires) {
		return entry, entry.err
	}
	entry, err := r.load(ctx, publicIdentifier)
	if ctx.Err() != nil {
		return cacheEntry{}, ctx.Err()
	}
	ttl := r.cacheTTL
	if err != nil {
		ttl = time.Second
	}
	entry.expires = r.now().Add(ttl)
	if err == nil && entry.deploymentExpiresAt != nil && entry.deploymentExpiresAt.Before(entry.expires) {
		entry.expires = *entry.deploymentExpiresAt
	}
	entry.err = err
	r.mu.Lock()
	if len(r.cache) >= 1024 {
		for key, value := range r.cache {
			if !r.now().Before(value.expires) {
				delete(r.cache, key)
			}
		}
		if len(r.cache) >= 1024 {
			for key := range r.cache {
				delete(r.cache, key)
				break
			}
		}
	}
	r.cache[publicIdentifier] = entry
	r.mu.Unlock()
	return entry, err
}

func (r *Router) load(ctx context.Context, publicIdentifier string) (cacheEntry, error) {
	deployment, err := r.resolver.GetByPublicIdentifier(ctx, publicIdentifier)
	if err != nil {
		if deployment.IsExpired(r.now()) {
			return cacheEntry{}, deployments.ErrExpired
		}
		return cacheEntry{}, err
	}
	if deployment.IsExpired(r.now()) || deployment.Status == deployments.StatusExpired {
		return cacheEntry{}, deployments.ErrExpired
	}
	if deployment.Status != deployments.StatusRunning || deployment.HostPort == nil {
		if deployment.IsExpired(r.now()) || deployment.Status == deployments.StatusExpired {
			return cacheEntry{}, deployments.ErrExpired
		}
		return cacheEntry{}, errors.New("deployment is not running")
	}
	host := r.upstreamHost
	if deployment.WorkerAddress != nil {
		u, err := url.Parse(*deployment.WorkerAddress)
		if err != nil || u.Scheme != "http" || u.Hostname() == "" {
			return cacheEntry{}, errors.New("invalid worker address")
		}
		host = u.Hostname()
	}
	if host == "" {
		return cacheEntry{}, errors.New("deployment has no worker location")
	}
	if *deployment.HostPort < 1 || *deployment.HostPort > 65535 {
		return cacheEntry{}, errors.New("invalid application port")
	}
	return cacheEntry{host: host, hostPort: *deployment.HostPort, deploymentExpiresAt: deployment.ExpiresAt}, nil
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

func (r *Router) publicIdentifierFromHost(host string) (string, bool) {
	host = normalizedHost(host)
	suffix := "." + r.publicBaseDomain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	publicIdentifier := strings.TrimSuffix(host, suffix)
	if strings.Contains(publicIdentifier, ".") || !publicIdentifierPattern.MatchString(publicIdentifier) {
		return "", false
	}
	return publicIdentifier, true
}

func normalizedHost(host string) string {
	if value, _, err := net.SplitHostPort(host); err == nil {
		host = value
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: message})
}
