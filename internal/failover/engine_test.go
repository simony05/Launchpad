package failover

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/simon/launchpad/internal/build"
	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/database"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/scheduler"
	"github.com/simon/launchpad/internal/worker"
	"github.com/simon/launchpad/internal/workers"
	"github.com/simon/launchpad/internal/workspace"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeFencer struct {
	calls int
	err   error
}

func (f *fakeFencer) Fence(context.Context, Target) error { f.calls++; return f.err }

type fakeWorker struct {
	worker.Client
	calls        int
	last         worker.StartRequest
	lostResponse bool
}

func (f *fakeWorker) Start(_ context.Context, r worker.StartRequest) (worker.StartResult, error) {
	f.calls++
	f.last = r
	if f.lostResponse {
		return worker.StartResult{}, errors.New("lost response")
	}
	return worker.StartResult{ImageName: build.ImageName(r.DeploymentID, r.Version), Container: &containers.Container{ID: "ab12cd34ef56", InternalPort: 8000, HostPort: 32781}}, nil
}
func (f *fakeWorker) Status(_ context.Context, id string, version int) (worker.Status, error) {
	return worker.Status{DeploymentID: id, Version: version, Running: true, ContainerID: "ab12cd34ef56", HostPort: 32781}, nil
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	connection := os.Getenv("LAUNCHPAD_TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("set LAUNCHPAD_TEST_DATABASE_URL to test failover transactions")
	}
	ctx := context.Background()
	admin, err := database.Open(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	schema := `"failover_` + uuid.NewString() + `"`
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(connection)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close(); _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestFailoverGraceFencingAndReplay(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	registry := workers.Postgres{Pool: pool}
	repo := deployments.NewPostgresRepository(pool)
	a := workers.Report{ID: uuid.NewString(), InstanceID: "i-12345678", Hostname: "a", Address: "http://10.0.0.1:8090", Healthy: true, TotalCPU: 4, AvailableCPU: 4, TotalMemory: 2 << 30, AvailableMemory: 2 << 30}
	b := a
	b.ID = uuid.NewString()
	b.InstanceID = "i-87654321"
	b.Address = "http://10.0.0.2:8090"
	b.AvailableMemory = 1 << 30
	for _, r := range []workers.Report{a, b} {
		if err := registry.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	files := workspace.Files{"app.py": "from fastapi import FastAPI\napp=FastAPI()", "requirements.txt": "fastapi"}
	d, err := repo.Create(ctx, deployments.CreateInput{Name: "failover", Runtime: "python", Files: files})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkBuilding(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	placement := scheduler.Postgres{Pool: pool, HeartbeatTimeout: 45 * time.Second}
	if assigned, err := placement.Reserve(ctx, d.ID, containers.Limits{CPUs: "0.5", Memory: "256m"}); err != nil || assigned.WorkerID != a.ID {
		t.Fatalf("assignment %+v %v", assigned, err)
	}
	fencer := &fakeFencer{err: ErrFencingPending}
	client := &fakeWorker{lostResponse: true}
	invalidations := 0
	engine := Engine{Pool: pool, Fencer: fencer, Client: func(address string) (worker.Client, error) {
		if address != b.Address {
			t.Errorf("wrong destination %s", address)
		}
		return client, nil
	}, Grace: 180 * time.Second, HeartbeatTimeout: 45 * time.Second, OperationTimeout: time.Second, MaxAttempts: 3, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Invalidate: func(string) { invalidations++ }}
	age := func(seconds int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE workers SET last_heartbeat=clock_timestamp()-($2 * interval '1 second') WHERE id=$1`, a.ID, seconds); err != nil {
			t.Fatal(err)
		}
	}
	tick := func() {
		t.Helper()
		if err := engine.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	age(60)
	tick()
	if fencer.calls != 0 {
		t.Fatal("fenced during grace")
	}
	if err := registry.Record(ctx, a); err != nil {
		t.Fatal(err)
	}
	tick()
	if fencer.calls != 0 {
		t.Fatal("fenced recovered heartbeat")
	}
	age(200)
	tick()
	if fencer.calls != 1 || client.calls != 0 {
		t.Fatal("started before fencing confirmed")
	}
	if err := registry.Record(ctx, a); err == nil {
		t.Fatal("quarantined worker revived")
	}
	fencer.err = nil
	// No capacity keeps the old assignment without consuming failover attempts.
	b.Healthy = false
	if err := registry.Record(ctx, b); err != nil {
		t.Fatal(err)
	}
	tick()
	pending, err := repo.Get(ctx, d.ID)
	if err != nil || pending.Status != deployments.StatusRecovering || pending.Version != 1 || pending.FailoverAttempts != 0 {
		t.Fatalf("waiting %+v %v", pending, err)
	}
	b.Healthy = true
	if err := registry.Record(ctx, b); err != nil {
		t.Fatal(err)
	}
	tick()
	pending, err = repo.Get(ctx, d.ID)
	if err != nil || pending.Version != 2 || pending.Status != deployments.StatusRecovering || client.calls != 1 || client.last.Files["app.py"] != files["app.py"] {
		t.Fatalf("recovery %+v %v calls %d", pending, err, client.calls)
	}
	// Lost start response is reconciled from live container state after restart.
	tick()
	got, err := repo.Get(ctx, d.ID)
	if err != nil || got.Status != deployments.StatusRunning || got.Version != 2 || *got.WorkerID != b.ID || got.FailoverAttempts != 1 || *got.PublicIdentifier != *d.PublicIdentifier || client.calls != 1 || invalidations == 0 {
		t.Fatalf("recovered %+v %v calls %d", got, err, client.calls)
	}
	// Late requests from the old generation cannot release or stop the replacement.
	if err := placement.Release(ctx, d.ID, 1); err != nil {
		t.Fatal(err)
	}
	var reserved bool
	if err := pool.QueryRow(ctx, `SELECT capacity_reserved FROM deployments WHERE id=$1`, d.ID).Scan(&reserved); err != nil || !reserved {
		t.Fatalf("reservation lost %v", err)
	}
	if _, err := repo.MarkStopped(ctx, d.ID, 1); !errors.Is(err, deployments.ErrInvalidState) {
		t.Fatalf("stale delete accepted: %v", err)
	}
	if _, err := repo.FailBuild(ctx, d.ID, "old", "old"); !errors.Is(err, deployments.ErrInvalidState) {
		t.Fatalf("stale build accepted: %v", err)
	}
	tick()
	if client.calls != 1 {
		t.Fatal("completed recovery repeated")
	}
}

func TestFailoverMissingSource(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	repo := deployments.NewPostgresRepository(pool)
	r := workers.Report{ID: uuid.NewString(), Hostname: "old", Address: "http://10.0.0.1:8090", Healthy: true, TotalCPU: 4, AvailableCPU: 4, TotalMemory: 1 << 30, AvailableMemory: 1 << 30}
	if err := (workers.Postgres{Pool: pool}).Record(ctx, r); err != nil {
		t.Fatal(err)
	}
	d, err := repo.Create(ctx, deployments.CreateInput{Name: "legacy", Runtime: "python"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workers SET recovery_state='FENCED' WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE deployments SET worker_id=$2,worker_address=$3,status='RUNNING',reserved_cpu=0.5,reserved_memory=268435456 WHERE id=$1`, d.ID, r.ID, r.Address); err != nil {
		t.Fatal(err)
	}
	e := Engine{Pool: pool, MaxAttempts: 3, HeartbeatTimeout: 45 * time.Second, Invalidate: func(string) {}}
	if _, err := e.prepare(ctx); err == nil {
		t.Fatal("missing source accepted")
	}
	got, err := repo.Get(ctx, d.ID)
	if err != nil || got.Status != deployments.StatusFailed || got.RecoveryError == nil {
		t.Fatalf("%+v %v", got, err)
	}
	files, _ := json.Marshal(workspace.Files{"app.py": "app = 1", "requirements.txt": ""})
	if _, err := pool.Exec(ctx, `UPDATE deployments SET status='RUNNING',source_files=$2,failover_attempts=3 WHERE id=$1`, d.ID, files); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prepare(ctx); err == nil {
		t.Fatal("exhausted recovery budget accepted")
	}
	got, err = repo.Get(ctx, d.ID)
	if err != nil || got.Status != deployments.StatusFailed || got.RecoveryError == nil || !strings.Contains(*got.RecoveryError, "limit exhausted") {
		t.Fatalf("limit: %+v %v", got, err)
	}
}
