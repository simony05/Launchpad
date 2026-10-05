package workers

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/simon/launchpad/internal/database"
)

func TestPostgresHeartbeatLifecycle(t *testing.T) {
	url := os.Getenv("LAUNCHPAD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set LAUNCHPAD_TEST_DATABASE_URL to a disposable PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := Postgres{Pool: pool}
	report := validReport()
	report.ID = uuid.NewString()
	defer pool.Exec(context.Background(), "DELETE FROM workers WHERE id=$1", report.ID)
	assertStatus := func(want string) {
		t.Helper()
		items, err := store.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range items {
			if w.ID == report.ID {
				if w.Status != want {
					t.Fatalf("status %s want %s", w.Status, want)
				}
				return
			}
		}
		t.Fatal("worker missing")
	}
	if err := store.Record(ctx, report); err != nil {
		t.Fatal(err)
	}
	assertStatus("HEALTHY")
	if _, err := pool.Exec(ctx, "UPDATE workers SET last_heartbeat=clock_timestamp()-interval '2 minutes' WHERE id=$1", report.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Expire(ctx, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	assertStatus("UNHEALTHY")
	if err := store.Record(ctx, report); err != nil {
		t.Fatal(err)
	}
	if err := store.Expire(ctx, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	assertStatus("HEALTHY")
	report.Healthy = false
	if err := store.Record(ctx, report); err != nil {
		t.Fatal(err)
	}
	assertStatus("UNHEALTHY")
}
