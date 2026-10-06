package config

import (
	"testing"
	"time"
)

func TestFailoverConfiguration(t *testing.T) {
	t.Setenv("LAUNCHPAD_WORKER_RECOVERY_ENABLED", "false")
	t.Setenv("LAUNCHPAD_WORKER_RECOVERY_GRACE", "180s")
	t.Setenv("LAUNCHPAD_WORKER_RECOVERY_INTERVAL", "10s")
	t.Setenv("LAUNCHPAD_WORKER_RECOVERY_MAX_ATTEMPTS", "3")
	t.Setenv("AWS_REGION", "")
	c, err := LoadFailover(45 * time.Second)
	if err != nil || c.Enabled {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("LAUNCHPAD_WORKER_RECOVERY_ENABLED", "true")
	if _, err := LoadFailover(45 * time.Second); err == nil {
		t.Fatal("region required")
	}
	t.Setenv("AWS_REGION", "us-east-1")
	if _, err := LoadFailover(45 * time.Second); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAUNCHPAD_WORKER_RECOVERY_GRACE", "45s")
	if _, err := LoadFailover(45 * time.Second); err == nil {
		t.Fatal("grace must exceed liveness timeout")
	}
}
