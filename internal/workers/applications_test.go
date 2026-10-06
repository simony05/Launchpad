package workers

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/simon/launchpad/internal/database"
	"github.com/simon/launchpad/internal/deployments"
)

func TestPersistentRecoveryBudget(t *testing.T) {
	connection := os.Getenv("LAUNCHPAD_TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("set LAUNCHPAD_TEST_DATABASE_URL to a disposable PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	report := validReport()
	report.ID = uuid.NewString()
	if err := (Postgres{Pool: pool}).Record(ctx, report); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(context.Background(), "DELETE FROM workers WHERE id=$1", report.ID)
	repo := deployments.NewPostgresRepository(pool)
	d, err := repo.Create(ctx, deployments.CreateInput{Name: "recovery-test", Runtime: "python", HealthPath: "/health"})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(context.Background(), "DELETE FROM deployments WHERE id=$1", d.ID)
	cid := "ab12cd34ef56"
	if _, err := pool.Exec(ctx, `UPDATE deployments SET status='RUNNING',worker_id=$2,container_id=$3 WHERE id=$1`, d.ID, report.ID, cid); err != nil {
		t.Fatal(err)
	}
	o := Observation{DeploymentID: d.ID, ContainerID: cid, Version: 1, State: "EXITED", Message: "exit code 137", ExitCode: 137, OOMKilled: true, RequestRestart: true, LogTail: "Traceback: crash"}
	for attempt := 0; attempt < 3; attempt++ {
		// A new Applications value simulates a restarted control plane; counts persist.
		a := Applications{Pool: pool, MaxRestarts: 3, Cooldown: 30 * time.Second}
		o.ExpectedAttempts = attempt
		decision, err := a.Observe(ctx, report.ID, o)
		if err != nil || !decision.Restart {
			t.Fatalf("attempt %d %+v %v", attempt, decision, err)
		}
		if duplicate, err := a.Observe(ctx, report.ID, o); err != nil || duplicate.Restart {
			t.Fatalf("duplicate claim: %+v %v", duplicate, err)
		}
		if attempt < 2 {
			o.ExpectedAttempts++
			if blocked, err := a.Observe(ctx, report.ID, o); err != nil || blocked.Restart {
				t.Fatalf("cooldown ignored: %+v %v", blocked, err)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE deployments SET next_restart_at=clock_timestamp()-interval '1 second' WHERE id=$1`, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	o.ExpectedAttempts = 3
	a := Applications{Pool: pool, MaxRestarts: 3, Cooldown: 30 * time.Second}
	decision, err := a.Observe(ctx, report.ID, o)
	if err != nil || decision.Restart {
		t.Fatalf("limit ignored: %+v %v", decision, err)
	}
	saved, err := repo.Get(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != deployments.StatusFailed || saved.RestartAttempts != 3 || saved.RuntimeError == nil || saved.LastExitCode == nil || *saved.LastExitCode != 137 || !saved.OOMKilled || saved.RuntimeLogs != "Traceback: crash" {
		t.Fatalf("failure not persisted: %+v", saved)
	}
	if _, err := repo.MarkStopped(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	o.State = "RUNNING"
	o.RequestRestart = false
	if _, err := a.Observe(ctx, report.ID, o); err != nil {
		t.Fatal(err)
	}
	saved, err = repo.Get(ctx, d.ID)
	if err != nil || saved.Status != deployments.StatusStopped {
		t.Fatal("late health report resurrected stopped deployment")
	}
}
