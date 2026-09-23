package config

import (
	"log/slog"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("MINICLOUD_DATABASE_URL", "postgres://minicloud:minicloud@localhost:5432/minicloud")
	t.Setenv("MINICLOUD_WORKSPACE_ROOT", "/tmp/minicloud-workspaces")
	t.Setenv("MINICLOUD_HOST", "")
	t.Setenv("MINICLOUD_PORT", "")
	t.Setenv("MINICLOUD_LOG_LEVEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Host != "0.0.0.0" || cfg.Port != 8080 || cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("Load() = %#v, want defaults", cfg)
	}
}

func TestLoadRejectsInvalidPort(t *testing.T) {
	t.Setenv("MINICLOUD_DATABASE_URL", "postgres://minicloud:minicloud@localhost:5432/minicloud")
	t.Setenv("MINICLOUD_WORKSPACE_ROOT", "/tmp/minicloud-workspaces")
	t.Setenv("MINICLOUD_PORT", "not-a-port")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRejectsInvalidLogLevel(t *testing.T) {
	t.Setenv("MINICLOUD_DATABASE_URL", "postgres://minicloud:minicloud@localhost:5432/minicloud")
	t.Setenv("MINICLOUD_WORKSPACE_ROOT", "/tmp/minicloud-workspaces")
	t.Setenv("MINICLOUD_LOG_LEVEL", "verbose")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadSetsLogLevel(t *testing.T) {
	t.Setenv("MINICLOUD_DATABASE_URL", "postgres://minicloud:minicloud@localhost:5432/minicloud")
	t.Setenv("MINICLOUD_WORKSPACE_ROOT", "/tmp/minicloud-workspaces")
	t.Setenv("MINICLOUD_LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelDebug)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("MINICLOUD_WORKSPACE_ROOT", "/tmp/minicloud-workspaces")
	t.Setenv("MINICLOUD_DATABASE_URL", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRequiresAbsoluteWorkspaceRoot(t *testing.T) {
	t.Setenv("MINICLOUD_DATABASE_URL", "postgres://minicloud:minicloud@localhost:5432/minicloud")
	t.Setenv("MINICLOUD_WORKSPACE_ROOT", "workspaces")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRejectsInvalidBuildTimeout(t *testing.T) {
	t.Setenv("MINICLOUD_DATABASE_URL", "postgres://minicloud:minicloud@localhost:5432/minicloud")
	t.Setenv("MINICLOUD_WORKSPACE_ROOT", "/tmp/minicloud-workspaces")
	t.Setenv("MINICLOUD_BUILD_TIMEOUT_SECONDS", "10")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRejectsInvalidApplicationMemory(t *testing.T) {
	t.Setenv("MINICLOUD_DATABASE_URL", "postgres://minicloud:minicloud@localhost:5432/minicloud")
	t.Setenv("MINICLOUD_WORKSPACE_ROOT", "/tmp/minicloud-workspaces")
	t.Setenv("MINICLOUD_APP_MEMORY", "two-hundred-megabytes")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRejectsNonFiniteApplicationCPUs(t *testing.T) {
	t.Setenv("MINICLOUD_DATABASE_URL", "postgres://minicloud:minicloud@localhost:5432/minicloud")
	t.Setenv("MINICLOUD_WORKSPACE_ROOT", "/tmp/minicloud-workspaces")
	t.Setenv("MINICLOUD_APP_CPUS", "NaN")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRejectsRouterHostWithPort(t *testing.T) {
	t.Setenv("MINICLOUD_DATABASE_URL", "postgres://minicloud:minicloud@localhost:5432/minicloud")
	t.Setenv("MINICLOUD_WORKSPACE_ROOT", "/tmp/minicloud-workspaces")
	t.Setenv("MINICLOUD_ROUTER_UPSTREAM_HOST", "127.0.0.1:32781")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}
