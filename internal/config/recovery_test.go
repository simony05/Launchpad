package config

import "testing"

func TestRecoveryConfiguration(t *testing.T) {
	t.Setenv("LAUNCHPAD_APP_HEALTH_INTERVAL", "10s")
	t.Setenv("LAUNCHPAD_RESTART_COOLDOWN", "30s")
	for _, value := range []string{"0", "3", "10"} {
		t.Setenv("LAUNCHPAD_MAX_RESTART_ATTEMPTS", value)
		if _, err := LoadRecovery(); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"-1", "11", "forever"} {
		t.Setenv("LAUNCHPAD_MAX_RESTART_ATTEMPTS", value)
		if _, err := LoadRecovery(); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}
