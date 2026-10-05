package config

import "testing"

func TestWorkerRequiresPrivateBindIP(t *testing.T) {
	t.Setenv("LAUNCHPAD_WORKSPACE_ROOT", "/workspaces")
	t.Setenv("LAUNCHPAD_WORKER_TOKEN", "test")
	for _, ip := range []string{"", "0.0.0.0", "8.8.8.8", "127.0.0.1"} {
		t.Setenv("LAUNCHPAD_APP_BIND_IP", ip)
		if _, err := LoadWorker(); err == nil {
			t.Fatalf("accepted %q", ip)
		}
	}
	t.Setenv("LAUNCHPAD_APP_BIND_IP", "172.31.1.20")
	if _, err := LoadWorker(); err != nil {
		t.Fatal(err)
	}
}
