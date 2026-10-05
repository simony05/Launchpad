package config

import (
	"testing"
	"time"
)

func TestRegistryTiming(t *testing.T) {
	t.Setenv("LAUNCHPAD_HEARTBEAT_TIMEOUT", "45s")
	t.Setenv("LAUNCHPAD_HEARTBEAT_CHECK_INTERVAL", "5s")
	cfg, err := LoadRegistry()
	if err != nil || cfg.Timeout != 45*time.Second {
		t.Fatalf("%+v %v", cfg, err)
	}
	for _, value := range []string{"0s", "-1s", "garbage", "2h"} {
		t.Setenv("LAUNCHPAD_HEARTBEAT_TIMEOUT", value)
		if _, err := LoadRegistry(); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
	t.Setenv("LAUNCHPAD_HEARTBEAT_TIMEOUT", "1s")
	if _, err := LoadRegistry(); err == nil {
		t.Fatal("accepted check interval greater than timeout")
	}
}
func TestHeartbeatIdentity(t *testing.T) {
	t.Setenv("LAUNCHPAD_WORKER_ID", "8bb34af2-396c-4b37-8905-1b93c6677a1d")
	t.Setenv("LAUNCHPAD_CONTROL_PLANE_URL", "http://10.0.0.1:8091")
	t.Setenv("LAUNCHPAD_WORKER_ADVERTISE_URL", "http://10.0.0.2:8090")
	t.Setenv("LAUNCHPAD_HEARTBEAT_INTERVAL", "10s")
	if _, err := LoadHeartbeat(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAUNCHPAD_WORKER_ID", "")
	if _, err := LoadHeartbeat(); err == nil {
		t.Fatal("accepted missing stable identity")
	}
}
