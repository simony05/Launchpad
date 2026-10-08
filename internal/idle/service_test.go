package idle

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/simon/launchpad/internal/database"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/worker"
	"github.com/simon/launchpad/internal/workers"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

type idleFake struct {
	mu            sync.Mutex
	wakes, sleeps int
	fail          bool
}

func (f *idleFake) Idle(_ context.Context, r worker.IdleRequest) (worker.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return worker.Status{}, errors.New("worker unavailable")
	}
	if r.Action == "wake" {
		f.wakes++
	} else {
		f.sleeps++
	}
	return worker.Status{DeploymentID: r.DeploymentID, Version: r.Version, ContainerID: r.ContainerID, Running: r.Action == "wake", HostPort: 32782}, nil
}
func fixture(t *testing.T) (*Service, deployments.Deployment, *idleFake) {
	t.Helper()
	connection := os.Getenv("LAUNCHPAD_TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("set LAUNCHPAD_TEST_DATABASE_URL for idle integration tests")
	}
	ctx := context.Background()
	admin, err := database.Open(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	schema := `"idle_` + uuid.NewString() + `"`
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
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
	report := workers.Report{ID: uuid.NewString(), Hostname: "worker", Address: "http://10.0.0.1:8090", Healthy: true, TotalCPU: 4, AvailableCPU: 4, TotalMemory: 2 << 30, AvailableMemory: 2 << 30}
	if err := (workers.Postgres{Pool: pool}).Record(ctx, report); err != nil {
		t.Fatal(err)
	}
	repo := deployments.NewPostgresRepository(pool)
	d, err := repo.Create(ctx, deployments.CreateInput{Name: "idle", Runtime: "python"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE deployments SET status='RUNNING',worker_id=$2,worker_address=$3,container_id='abcdef123456',host_port=32781,internal_port=8000,reserved_cpu=0.5,reserved_memory=268435456,capacity_reserved=TRUE,last_request_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, d.ID, report.ID, report.Address); err != nil {
		t.Fatal(err)
	}
	d, err = repo.Get(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	f := &idleFake{}
	s := &Service{Pool: pool, Repository: repo, Context: ctx, Client: func(string) (worker.IdleClient, error) { return f, nil }, IdleTimeout: time.Minute, OperationTimeout: time.Second, HeartbeatTimeout: 45 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Invalidate: func(string) {}, Probe: func(context.Context, string, int, string) error { return nil }}
	return s, d, f
}
func TestSleepConcurrentWakeAndActiveRequest(t *testing.T) {
	s, d, f := fixture(t)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sleeping, err := s.Repository.Get(ctx, d.ID)
	if err != nil || sleeping.Status != deployments.StatusSleeping || *sleeping.ContainerID != *d.ContainerID || sleeping.HostPort != nil {
		t.Fatalf("sleep %+v %v", sleeping, err)
	}
	var reserved bool
	if err := s.Pool.QueryRow(ctx, `SELECT capacity_reserved FROM deployments WHERE id=$1`, d.ID).Scan(&reserved); err != nil || reserved {
		t.Fatal("sleep did not release capacity", err)
	}
	if !s.AllowsHost(ctx, *d.PublicIdentifier) || f.wakes != 0 {
		t.Fatal("certificate authorization woke app or rejected sleeper")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	releases := make(chan func(), 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.Acquire(ctx, *d.PublicIdentifier)
			errs <- err
			if err == nil {
				releases <- release
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(releases)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.wakes != 1 || f.sleeps != 1 {
		t.Fatalf("duplicate lifecycle: wakes %d sleeps %d", f.wakes, f.sleeps)
	}
	running, err := s.Repository.Get(ctx, d.ID)
	if err != nil || running.Status != deployments.StatusRunning || *running.HostPort != 32782 || running.ColdStartMS == nil {
		t.Fatalf("wake %+v %v", running, err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE deployments SET last_request_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.sleeps != 1 {
		t.Fatal("active requests were stopped")
	}
	for release := range releases {
		release()
		release()
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.sleeps != 1 {
		t.Fatal("request completion did not refresh idle clock")
	}
}
func TestWakeCapacityAndDeletion(t *testing.T) {
	s, d, f := fixture(t)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE workers SET available_cpu=0 WHERE id=$1`, *d.WorkerID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, *d.PublicIdentifier); err == nil || f.wakes != 0 {
		t.Fatal("woke without capacity")
	}
	if _, err := s.Repository.ClaimStop(ctx, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, *d.PublicIdentifier); err == nil {
		t.Fatal("deleting app was revived")
	}
}
func TestInterruptedSleepAndReadinessFailure(t *testing.T) {
	s, d, f := fixture(t)
	ctx := context.Background()
	f.fail = true
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := s.Repository.Get(ctx, d.ID)
	if err != nil || pending.Status != deployments.StatusSuspending || pending.IdleError == nil {
		t.Fatalf("pending %+v %v", pending, err)
	}
	f.fail = false
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	s.Probe = func(context.Context, string, int, string) error { return context.DeadlineExceeded }
	if _, err := s.Acquire(ctx, *d.PublicIdentifier); err == nil {
		t.Fatal("forwarded before readiness")
	}
	failed, err := s.Repository.Get(ctx, d.ID)
	if err != nil || failed.Status != deployments.StatusFailed || failed.IdleError == nil {
		t.Fatalf("failure %+v %v", failed, err)
	}
	if _, err := s.Acquire(ctx, *d.PublicIdentifier); err == nil || f.wakes != 1 {
		t.Fatal("failed wake loop")
	}
}
func TestFencedSleeperOnlyRecoversOnDemand(t *testing.T) {
	s, d, f := fixture(t)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE workers SET recovery_state='FENCED',status='UNHEALTHY' WHERE id=$1`, *d.WorkerID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sleeping, err := s.Repository.Get(ctx, d.ID)
	if err != nil || sleeping.Status != deployments.StatusSleeping {
		t.Fatal("idle app reactivated")
	}
	if _, err := s.Acquire(ctx, *d.PublicIdentifier); err == nil {
		t.Fatal("fenced app forwarded")
	}
	waking, err := s.Repository.Get(ctx, d.ID)
	if err != nil || waking.Status != deployments.StatusWaking || f.wakes != 0 {
		t.Fatalf("demand recovery %+v %v", waking, err)
	}
}

func TestRegistryRejectsStaleIdleEpoch(t *testing.T) {
	s, d, f := fixture(t)
	ctx := context.Background()
	f.fail = true
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	registry := workers.Handler(workers.Postgres{Pool: s.Pool}, "secret", workers.Applications{Pool: s.Pool})
	check := func() int {
		req := httptest.NewRequest("GET", "/internal/workers/"+*d.WorkerID+"/idle/"+d.ID+"?version=1&epoch=1&action=sleep", nil)
		req.Header.Set("Authorization", "Bearer secret")
		out := httptest.NewRecorder()
		registry.ServeHTTP(out, req)
		return out.Code
	}
	if code := check(); code != 204 {
		t.Fatalf("current operation rejected: %d", code)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE deployments SET idle_epoch=idle_epoch+1 WHERE id=$1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if code := check(); code != 409 {
		t.Fatalf("stale epoch accepted: %d", code)
	}
}
