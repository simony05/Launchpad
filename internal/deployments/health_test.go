package deployments

import "testing"

func TestValidateHealthPath(t *testing.T) {
	for _, path := range []string{"", "/health", "/ready?probe=1"} {
		if err := ValidateHealthPath(path); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"http://169.254.169.254/", "//example.com/", "health", "/health\n"} {
		if err := ValidateHealthPath(path); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}
