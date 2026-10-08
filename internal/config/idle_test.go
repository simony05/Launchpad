package config

import (
	"testing"
	"time"
)

func TestIdleConfig(t *testing.T) {
	t.Setenv("LAUNCHPAD_SCALE_TO_ZERO_ENABLED", "false")
	t.Setenv("LAUNCHPAD_IDLE_TIMEOUT", "15m")
	t.Setenv("LAUNCHPAD_IDLE_CHECK_INTERVAL", "30s")
	t.Setenv("LAUNCHPAD_COLD_START_TIMEOUT", "60s")
	c, err := LoadIdle()
	if err != nil || c.Enabled || c.Timeout != 15*time.Minute {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("LAUNCHPAD_SCALE_TO_ZERO_ENABLED", "true")
	if c, err := LoadIdle(); err != nil || !c.Enabled {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("LAUNCHPAD_COLD_START_TIMEOUT", "3m")
	if _, err := LoadIdle(); err == nil {
		t.Fatal("unbounded wake timeout")
	}
	t.Setenv("LAUNCHPAD_COLD_START_TIMEOUT", "0s")
	if _, err := LoadIdle(); err == nil {
		t.Fatal("zero wake timeout")
	}
}
