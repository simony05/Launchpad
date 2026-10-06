package scheduler

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/database"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/workers"
)

func TestRequirements(t *testing.T) {
	for _, test := range []struct {
		cpu, memory string
		valid       bool
		bytes       int64
	}{
		{"0.5", "256m", true, 256 << 20}, {"2", "1G", true, 1 << 30}, {"NaN", "1g", false, 0}, {"-1", "1g", false, 0}, {"1", "0m", false, 0}, {"1", "999999999999999999g", false, 0}, {"1", "1mb", false, 0},
	} {
		_, memory, err := Requirements(containers.Limits{CPUs: test.cpu, Memory: test.memory})
		if (err == nil) != test.valid || memory != test.bytes {
			t.Fatalf("%+v: %d %v", test, memory, err)
		}
	}
}

func TestPostgresPlacement(t *testing.T) {
	connection := os.Getenv("LAUNCHPAD_TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("set LAUNCHPAD_TEST_DATABASE_URL to test scheduling transactions")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := database.Open(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "schedule_" + uuid.New().String()
	schemaQuoted := `"` + schema + `"`
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schemaQuoted); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schemaQuoted+" CASCADE")
	config, err := pgxpool.ParseConfig(connection)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schemaQuoted
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s := Postgres{Pool: pool, HeartbeatTimeout: 45 * time.Second}
	registry := workers.Postgres{Pool: pool}
	repo := deployments.NewPostgresRepository(pool)
	a := workers.Report{ID: uuid.NewString(), Hostname: "a", Address: "http://10.0.0.1:8090", Healthy: true, TotalCPU: 4, AvailableCPU: 4, TotalMemory: 2 << 30, AvailableMemory: 2 << 30}
	b := a
	b.ID = uuid.NewString()
	b.Hostname = "b"
	b.Address = "http://10.0.0.2:8090"
	b.AvailableMemory = 1 << 30
	for _, w := range []workers.Report{a, b} {
		if err := registry.Record(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	create := func() string {
		t.Helper()
		d, err := repo.Create(ctx, deployments.CreateInput{Name: "schedule-test", Runtime: "python"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.MarkBuilding(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	limits := containers.Limits{CPUs: "0.5", Memory: "256m"}
	id := create()
	assignment, err := s.Reserve(ctx, id, limits)
	if err != nil || assignment.WorkerID != a.ID {
		t.Fatalf("largest memory: %+v %v", assignment, err)
	}
	saved, err := repo.Get(ctx, id)
	if err != nil || saved.WorkerID == nil || *saved.WorkerID != a.ID || *saved.WorkerAddress != a.Address {
		t.Fatalf("assignment not persisted: %+v %v", saved, err)
	}
	if _, err := s.Reserve(ctx, id, limits); err == nil {
		t.Fatal("duplicate reservation accepted")
	}
	if err := s.Release(ctx, id, 1); err != nil {
		t.Fatal(err)
	}
	// A fresh heartbeat cannot erase existing reservations.
	a.AvailableCPU = 0.5
	a.AvailableMemory = 256 << 20
	b.Healthy = false
	if err := registry.Record(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := registry.Record(ctx, b); err != nil {
		t.Fatal(err)
	}
	first, second := create(), create()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{first, second} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); _, err := s.Reserve(ctx, id, limits); results <- err }(id)
	}
	wg.Wait()
	close(results)
	successes, denied := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrNoCapacity) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || denied != 1 {
		t.Fatalf("concurrent placement: %d successes %d denied", successes, denied)
	}
	if err := registry.Record(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, create(), limits); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("heartbeat erased reservation: %v", err)
	}
	if err := s.Release(ctx, first, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, second, 1); err != nil {
		t.Fatal(err)
	}
	// Stale HEALTHY workers are rejected before the periodic monitor runs.
	if _, err := pool.Exec(ctx, `UPDATE workers SET last_heartbeat=clock_timestamp()-interval '2 minutes' WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, create(), limits); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("stale worker eligible: %v", err)
	}
	if err := registry.Record(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, create(), containers.Limits{CPUs: "1", Memory: "256m"}); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("CPU eligibility: %v", err)
	}
	if _, err := s.Reserve(ctx, create(), containers.Limits{CPUs: "0.5", Memory: "512m"}); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("memory eligibility: %v", err)
	}
}
