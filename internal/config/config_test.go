package config

import (
	"log/slog"
	"testing"
)

func setRequiredEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("LAUNCHPAD_DATABASE_URL", "postgres://launchpad:launchpad@localhost:5432/launchpad")
	t.Setenv("LAUNCHPAD_WORKSPACE_ROOT", "/tmp/launchpad-workspaces")
	t.Setenv("LAUNCHPAD_PUBLIC_BASE_DOMAIN", "apps.example.com")
}

func TestLoadDefaults(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("LAUNCHPAD_HOST", "")
	t.Setenv("LAUNCHPAD_PORT", "")
	t.Setenv("LAUNCHPAD_LOG_LEVEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Host != "0.0.0.0" || cfg.Port != 8080 || cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("Load() = %#v, want defaults", cfg)
	}
}

func TestLoadRejectsInvalidPort(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("LAUNCHPAD_PORT", "not-a-port")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRejectsInvalidLogLevel(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("LAUNCHPAD_LOG_LEVEL", "verbose")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadSetsLogLevel(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("LAUNCHPAD_LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelDebug)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("LAUNCHPAD_WORKSPACE_ROOT", "/tmp/launchpad-workspaces")
	t.Setenv("LAUNCHPAD_PUBLIC_BASE_DOMAIN", "apps.example.com")
	t.Setenv("LAUNCHPAD_DATABASE_URL", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRequiresAbsoluteWorkspaceRoot(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("LAUNCHPAD_WORKSPACE_ROOT", "workspaces")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRejectsInvalidBuildTimeout(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("LAUNCHPAD_BUILD_TIMEOUT_SECONDS", "10")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRejectsInvalidApplicationMemory(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("LAUNCHPAD_APP_MEMORY", "two-hundred-megabytes")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRejectsNonFiniteApplicationCPUs(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("LAUNCHPAD_APP_CPUS", "NaN")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRejectsRouterHostWithPort(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("LAUNCHPAD_ROUTER_UPSTREAM_HOST", "127.0.0.1:32781")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestLoadRequiresPublicBaseDomain(t *testing.T) {
	t.Setenv("LAUNCHPAD_DATABASE_URL", "postgres://launchpad:launchpad@localhost:5432/launchpad")
	t.Setenv("LAUNCHPAD_WORKSPACE_ROOT", "/tmp/launchpad-workspaces")
	t.Setenv("LAUNCHPAD_PUBLIC_BASE_DOMAIN", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}
