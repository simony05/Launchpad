package config

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

const (
	defaultHost               = "0.0.0.0"
	defaultPort               = 8080
	defaultLogLevel           = slog.LevelInfo
	defaultBuildTimeout       = 5 * 60
	defaultStartTimeout       = 30
	defaultAppCPUs            = "0.5"
	defaultAppMemory          = "256m"
	defaultRouterUpstreamHost = "host.docker.internal"
	defaultRouterCacheTTL     = 5
)

// Config contains runtime configuration for the control plane.
type Config struct {
	Host                  string
	Port                  int
	LogLevel              slog.Level
	DatabaseURL           string
	WorkspaceRoot         string
	BuildTimeoutSeconds   int
	StartTimeoutSeconds   int
	AppCPUs               string
	AppMemory             string
	RouterUpstreamHost    string
	RouterCacheTTLSeconds int
}

// Load reads control-plane configuration from environment variables.
func Load() (Config, error) {
	cfg := Config{
		Host:                  envOrDefault("MINICLOUD_HOST", defaultHost),
		Port:                  defaultPort,
		LogLevel:              defaultLogLevel,
		DatabaseURL:           os.Getenv("MINICLOUD_DATABASE_URL"),
		WorkspaceRoot:         os.Getenv("MINICLOUD_WORKSPACE_ROOT"),
		BuildTimeoutSeconds:   defaultBuildTimeout,
		StartTimeoutSeconds:   defaultStartTimeout,
		AppCPUs:               envOrDefault("MINICLOUD_APP_CPUS", defaultAppCPUs),
		AppMemory:             envOrDefault("MINICLOUD_APP_MEMORY", defaultAppMemory),
		RouterUpstreamHost:    envOrDefault("MINICLOUD_ROUTER_UPSTREAM_HOST", defaultRouterUpstreamHost),
		RouterCacheTTLSeconds: defaultRouterCacheTTL,
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("MINICLOUD_DATABASE_URL must be set")
	}
	if cfg.WorkspaceRoot == "" {
		return Config{}, fmt.Errorf("MINICLOUD_WORKSPACE_ROOT must be set")
	}
	if !filepath.IsAbs(cfg.WorkspaceRoot) {
		return Config{}, fmt.Errorf("MINICLOUD_WORKSPACE_ROOT must be an absolute path")
	}
	if rawTimeout := os.Getenv("MINICLOUD_BUILD_TIMEOUT_SECONDS"); rawTimeout != "" {
		timeout, err := strconv.Atoi(rawTimeout)
		if err != nil || timeout < 30 || timeout > 1800 {
			return Config{}, fmt.Errorf("MINICLOUD_BUILD_TIMEOUT_SECONDS must be an integer between 30 and 1800")
		}
		cfg.BuildTimeoutSeconds = timeout
	}
	if rawTimeout := os.Getenv("MINICLOUD_START_TIMEOUT_SECONDS"); rawTimeout != "" {
		timeout, err := strconv.Atoi(rawTimeout)
		if err != nil || timeout < 5 || timeout > 300 {
			return Config{}, fmt.Errorf("MINICLOUD_START_TIMEOUT_SECONDS must be an integer between 5 and 300")
		}
		cfg.StartTimeoutSeconds = timeout
	}
	if cpus, err := strconv.ParseFloat(cfg.AppCPUs, 64); err != nil || math.IsNaN(cpus) || math.IsInf(cpus, 0) || cpus < 0.1 || cpus > 64 {
		return Config{}, fmt.Errorf("MINICLOUD_APP_CPUS must be a number between 0.1 and 64")
	}
	if !memoryPattern.MatchString(cfg.AppMemory) {
		return Config{}, fmt.Errorf("MINICLOUD_APP_MEMORY must be a whole number followed by m or g")
	}
	if !hostnamePattern.MatchString(cfg.RouterUpstreamHost) {
		return Config{}, fmt.Errorf("MINICLOUD_ROUTER_UPSTREAM_HOST must be a hostname or IPv4 address without a port")
	}
	if rawTTL := os.Getenv("MINICLOUD_ROUTER_CACHE_TTL_SECONDS"); rawTTL != "" {
		ttl, err := strconv.Atoi(rawTTL)
		if err != nil || ttl < 1 || ttl > 60 {
			return Config{}, fmt.Errorf("MINICLOUD_ROUTER_CACHE_TTL_SECONDS must be an integer between 1 and 60")
		}
		cfg.RouterCacheTTLSeconds = ttl
	}

	if rawPort := os.Getenv("MINICLOUD_PORT"); rawPort != "" {
		port, err := strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("MINICLOUD_PORT must be an integer between 1 and 65535")
		}
		cfg.Port = port
	}

	if rawLevel := os.Getenv("MINICLOUD_LOG_LEVEL"); rawLevel != "" {
		level, ok := logLevels[rawLevel]
		if !ok {
			return Config{}, fmt.Errorf("MINICLOUD_LOG_LEVEL must be debug, info, warn, or error")
		}
		cfg.LogLevel = level
	}

	return cfg, nil
}

var memoryPattern = regexp.MustCompile(`^[1-9][0-9]*[mMgG]$`)
var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// Address returns the address on which the server should listen.
func (c Config) Address() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
