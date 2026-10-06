package scheduler

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

// Choose is used inside the shared placement lock by normal placement and failover.
func Choose(ctx context.Context, tx pgx.Tx, timeout time.Duration, cpu float64, memory int64, exclude string) (Assignment, error) {
	var a Assignment
	err := tx.QueryRow(ctx, `SELECT w.id::text,w.address FROM workers w
 LEFT JOIN LATERAL (SELECT COALESCE(SUM(reserved_cpu),0) cpu,COALESCE(SUM(reserved_memory),0) memory FROM deployments WHERE worker_id=w.id AND capacity_reserved) r ON true
 WHERE w.status='HEALTHY' AND w.recovery_state='ACTIVE' AND w.id::text<>$4
 AND w.last_heartbeat>=clock_timestamp()-($1 * interval '1 second')
 AND w.available_cpu-r.cpu >= $2 AND w.available_memory-r.memory >= $3
 ORDER BY w.available_memory-r.memory DESC,w.id LIMIT 1`, timeout.Seconds(), cpu, memory, exclude).Scan(&a.WorkerID, &a.Address)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNoCapacity
	}
	return a, err
}
