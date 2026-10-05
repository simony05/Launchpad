package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/simon/launchpad/internal/containers"
)

var ErrNoCapacity = errors.New("no healthy worker has sufficient CPU and memory")

type Assignment struct {
	WorkerID string
	Address  string
}
type Scheduler interface {
	Reserve(context.Context, string, containers.Limits) (Assignment, error)
	Release(context.Context, string) error
}

type Postgres struct {
	Pool             *pgxpool.Pool
	HeartbeatTimeout time.Duration
}

func Requirements(limits containers.Limits) (float64, int64, error) {
	cpu, err := strconv.ParseFloat(limits.CPUs, 64)
	if err != nil || math.IsNaN(cpu) || math.IsInf(cpu, 0) || cpu <= 0 {
		return 0, 0, errors.New("invalid CPU requirement")
	}
	memory := strings.ToLower(limits.Memory)
	if len(memory) < 2 {
		return 0, 0, errors.New("invalid memory requirement")
	}
	multiplier := int64(1024 * 1024)
	switch memory[len(memory)-1] {
	case 'm':
	case 'g':
		multiplier *= 1024
	default:
		return 0, 0, errors.New("memory must use m or g units")
	}
	amount, err := strconv.ParseInt(memory[:len(memory)-1], 10, 64)
	if err != nil || amount <= 0 || amount > math.MaxInt64/multiplier {
		return 0, 0, errors.New("invalid memory requirement")
	}
	return cpu, amount * multiplier, nil
}

// Reserve serializes short placement transactions, not builds. Reservations
// remain durable across control-plane restarts and heartbeat updates.
func (p Postgres) Reserve(ctx context.Context, id string, limits containers.Limits) (Assignment, error) {
	cpu, memory, err := Requirements(limits)
	if err != nil {
		return Assignment{}, err
	}
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return Assignment{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(110011)`); err != nil {
		return Assignment{}, err
	}
	var status string
	var assigned *string
	if err := tx.QueryRow(ctx, `SELECT status::text,worker_id::text FROM deployments WHERE id=$1 FOR UPDATE`, id).Scan(&status, &assigned); err != nil {
		return Assignment{}, err
	}
	if status != "BUILDING" || assigned != nil {
		return Assignment{}, errors.New("deployment is not awaiting placement")
	}
	var a Assignment
	// Subtract all outstanding reservations from reported free capacity. This is
	// conservative for already-running containers but safe with stale snapshots.
	err = tx.QueryRow(ctx, `SELECT w.id::text,w.address FROM workers w
 LEFT JOIN LATERAL (
   SELECT COALESCE(SUM(reserved_cpu),0) AS cpu,COALESCE(SUM(reserved_memory),0) AS memory
   FROM deployments WHERE worker_id=w.id AND capacity_reserved
 ) r ON true
 WHERE w.status='HEALTHY'
 AND w.last_heartbeat >= clock_timestamp()-($1 * interval '1 second')
 AND w.available_cpu-r.cpu >= $2 AND w.available_memory-r.memory >= $3
 ORDER BY w.available_memory-r.memory DESC,w.id ASC LIMIT 1`, p.HeartbeatTimeout.Seconds(), cpu, memory).Scan(&a.WorkerID, &a.Address)
	if errors.Is(err, pgx.ErrNoRows) {
		return Assignment{}, ErrNoCapacity
	}
	if err != nil {
		return Assignment{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE deployments SET worker_id=$2,worker_address=$3,reserved_cpu=$4,reserved_memory=$5,capacity_reserved=TRUE WHERE id=$1`, id, a.WorkerID, a.Address, cpu, memory)
	if err != nil {
		return Assignment{}, fmt.Errorf("reserve worker capacity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Assignment{}, err
	}
	return a, nil
}

func (p Postgres) Release(ctx context.Context, id string) error {
	_, err := p.Pool.Exec(ctx, `UPDATE deployments SET capacity_reserved=FALSE WHERE id=$1`, id)
	return err
}
