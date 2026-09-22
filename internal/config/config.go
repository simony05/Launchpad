package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

const (
	defaultHost     = "0.0.0.0"
	defaultPort     = 8080
	defaultLogLevel = slog.LevelInfo
)

// Config contains runtime configuration for the control plane.
type Config struct {
	Host          string
	Port          int
	LogLevel      slog.Level
	DatabaseURL   string
	WorkspaceRoot string
}

// Load reads control-plane configuration from environment variables.
func Load() (Config, error) {
	cfg := Config{
		Host:          envOrDefault("MINICLOUD_HOST", defaultHost),
		Port:          defaultPort,
		LogLevel:      defaultLogLevel,
		DatabaseURL:   os.Getenv("MINICLOUD_DATABASE_URL"),
		WorkspaceRoot: os.Getenv("MINICLOUD_WORKSPACE_ROOT"),
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
