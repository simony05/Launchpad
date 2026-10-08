package idle

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/worker"
)

// Service coordinates one control-plane/router process. PostgreSQL holds durable
// lifecycle intent; bounded local stripes protect requests (including streams).
type Service struct {
	Pool                                            *pgxpool.Pool
	Repository                                      *deployments.PostgresRepository
	Client                                          func(string) (worker.IdleClient, error)
	IdleTimeout, OperationTimeout, HeartbeatTimeout time.Duration
	Logger                                          *slog.Logger
	Invalidate                                      func(string)
	Context                                         context.Context
	Probe                                           func(context.Context, string, int, string) error
	once                                            sync.Once
	locks                                           [128]chan struct{}
	active                                          [128]int
}

// Certificate authorization must not generate traffic or cold-start an app.
func (s *Service) AllowsHost(ctx context.Context, id string) bool {
	d, err := s.Repository.GetByPublicIdentifier(ctx, id)
	return err == nil && (d.Status == deployments.StatusRunning || d.Status == deployments.StatusSleeping || d.Status == deployments.StatusWaking || d.Status == deployments.StatusSuspending)
}
func (s *Service) slot(id string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % 128)
}
func (s *Service) lock(ctx context.Context, id string) (int, error) {
	s.once.Do(func() {
		for i := range s.locks {
			s.locks[i] = make(chan struct{}, 1)
		}
	})
	i := s.slot(id)
	select {
	case s.locks[i] <- struct{}{}:
		return i, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
func (s *Service) unlock(i int) { <-s.locks[i] }

// Acquire is called before reading or forwarding the incoming request body.
func (s *Service) Acquire(ctx context.Context, id string) (func(), error) {
	i, err := s.lock(ctx, id)
	if err != nil {
		return nil, err
	}
	defer s.unlock(i)
	if err := s.Context.Err(); err != nil {
		return nil, err
	}
	// Warm requests need only an activity write, not another metadata/location read.
	var runningID string
	var runningVersion int
	err = s.Pool.QueryRow(ctx, `UPDATE deployments SET last_request_at=clock_timestamp() WHERE public_identifier=$1 AND status='RUNNING' RETURNING id::text,version`, id).Scan(&runningID, &runningVersion)
	if err == nil {
		return s.retain(i, id, runningID, runningVersion), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	d, err := s.Repository.GetByPublicIdentifier(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.Status == deployments.StatusSuspending || d.Status == deployments.StatusSleeping || d.Status == deployments.StatusWaking {
		op, cancel := context.WithTimeout(s.Context, s.OperationTimeout)
		defer cancel()
		if d.Status == deployments.StatusSuspending {
			if err := s.transition(op, d, false); err != nil {
				return nil, err
			}
			d, err = s.Repository.Get(op, d.ID)
			if err != nil {
				return nil, err
			}
		}
		if err := s.transition(op, d, true); err != nil {
			return nil, err
		}
		d, err = s.Repository.Get(op, d.ID)
		if err != nil {
			return nil, err
		}
	}
	if d.Status != deployments.StatusRunning {
		return nil, errors.New("deployment is unavailable")
	}
	result, err := s.Pool.Exec(ctx, `UPDATE deployments SET last_request_at=clock_timestamp() WHERE id=$1 AND version=$2 AND status='RUNNING'`, d.ID, d.Version)
	if err != nil {
		return nil, err
	}
	if result.RowsAffected() != 1 {
		return nil, errors.New("deployment changed during request")
	}
	return s.retain(i, id, d.ID, d.Version), nil
}

func (s *Service) retain(i int, id, deploymentID string, version int) func() {
	s.active[i]++
	var once sync.Once
	return func() {
		once.Do(func() {
			finish, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = s.Pool.Exec(finish, `UPDATE deployments SET last_request_at=clock_timestamp() WHERE id=$1 AND version=$2 AND status='RUNNING'`, deploymentID, version)
			// Completion must always release the count, even during shutdown.
			slot, _ := s.lock(context.Background(), id)
			s.active[slot]--
			s.unlock(slot)
		})
	}
}

func (s *Service) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
				s.Logger.Error("idle scan", "error", err)
			}
		}
	}
}
func (s *Service) Tick(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, `SELECT public_identifier FROM deployments WHERE status IN ('SUSPENDING','WAKING') OR (status='RUNNING' AND worker_id IS NOT NULL AND container_id IS NOT NULL AND last_request_at<clock_timestamp()-($1 * interval '1 second')) ORDER BY last_request_at LIMIT 100`, s.IdleTimeout.Seconds())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		op, cancel := context.WithTimeout(ctx, s.OperationTimeout)
		i, err := s.lock(op, id)
		if err != nil {
			cancel()
			continue
		}
		if s.active[i] == 0 {
			d, err := s.Repository.GetByPublicIdentifier(op, id)
			if err == nil {
				err = s.transition(op, d, d.Status == deployments.StatusWaking)
			}
			if err != nil && ctx.Err() == nil {
				s.Logger.Warn("idle transition pending", "public_identifier", id, "error", err)
			}
		}
		s.unlock(i)
		cancel()
	}
	return nil
}

func (s *Service) transition(ctx context.Context, d deployments.Deployment, wake bool) error {
	if d.WorkerID == nil || d.WorkerAddress == nil || d.ContainerID == nil {
		return errors.New("deployment has no retained container")
	}
	var state string
	var healthy bool
	if err := s.Pool.QueryRow(ctx, `SELECT recovery_state,status='HEALTHY' AND last_heartbeat>=clock_timestamp()-($2 * interval '1 second') FROM workers WHERE id=$1`, *d.WorkerID, s.HeartbeatTimeout.Seconds()).Scan(&state, &healthy); err != nil {
		return err
	}
	if state == "FENCED" {
		if wake {
			_, err := s.Pool.Exec(ctx, `UPDATE deployments SET status='WAKING' WHERE id=$1 AND version=$2 AND status='SLEEPING'`, d.ID, d.Version)
			if err != nil {
				return err
			}
			return errors.New("worker fenced; waiting for failure recovery")
		}
		_, err := s.Pool.Exec(ctx, `UPDATE deployments SET status='SLEEPING',capacity_reserved=FALSE,host_port=NULL WHERE id=$1 AND version=$2 AND status='SUSPENDING'`, d.ID, d.Version)
		s.Invalidate(*d.PublicIdentifier)
		return err
	}
	if state != "ACTIVE" || !healthy {
		return errors.New("worker unavailable")
	}
	desired := "SUSPENDING"
	action := "sleep"
	if wake {
		desired = "WAKING"
		action = "wake"
	}
	if wake && d.Status == deployments.StatusSleeping {
		if err := s.reserveWake(ctx, d); err != nil {
			return err
		}
	} else if !wake && d.Status == deployments.StatusRunning {
		result, err := s.Pool.Exec(ctx, `UPDATE deployments SET status='SUSPENDING',idle_epoch=idle_epoch+1,idle_attempts=0,idle_error=NULL WHERE id=$1 AND version=$2 AND status='RUNNING' AND last_request_at<clock_timestamp()-($3 * interval '1 second') AND (health_state IS NULL OR health_state='RUNNING') AND (next_restart_at IS NULL OR next_restart_at<clock_timestamp())`, d.ID, d.Version, s.IdleTimeout.Seconds())
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return nil
		}
	} else if string(d.Status) != desired {
		return errors.New("deployment cannot change idle state")
	}
	d, err := s.Repository.Get(ctx, d.ID)
	if err != nil {
		return err
	}
	if string(d.Status) != desired {
		return errors.New("idle state superseded")
	}
	s.Invalidate(*d.PublicIdentifier)
	var attempt int
	err = s.Pool.QueryRow(ctx, `UPDATE deployments SET idle_attempts=idle_attempts+1 WHERE id=$1 AND version=$2 AND idle_epoch=$3 AND status::text=$4 RETURNING idle_attempts`, d.ID, d.Version, d.IdleEpoch, desired).Scan(&attempt)
	if err != nil {
		return err
	}
	if attempt > 3 {
		return s.recordError(ctx, d, "idle operation retry limit exhausted; inspect retained container", true)
	}
	started := time.Now()
	client, err := s.Client(*d.WorkerAddress)
	if err != nil {
		return s.recordError(ctx, d, err.Error(), false)
	}
	status, err := client.Idle(ctx, worker.IdleRequest{DeploymentID: d.ID, Version: d.Version, Epoch: d.IdleEpoch, ContainerID: *d.ContainerID, Action: action})
	if err != nil {
		return s.recordError(ctx, d, err.Error(), false)
	}
	if status.DeploymentID != d.ID || status.Version != d.Version || status.ContainerID != *d.ContainerID || status.Running != wake {
		return s.recordError(ctx, d, "unexpected worker lifecycle response", false)
	}
	if wake {
		if status.HostPort < 1 || status.HostPort > 65535 {
			return s.recordError(ctx, d, "invalid wake port", true)
		}
		probe := s.Probe
		if probe == nil {
			probe = s.ready
		}
		if err := probe(ctx, *d.WorkerAddress, status.HostPort, d.HealthPath); err != nil {
			if s.Context.Err() != nil {
				return s.Context.Err()
			}
			check, cancel := context.WithTimeout(s.Context, 5*time.Second)
			defer cancel()
			var workerHealthy bool
			checkErr := s.Pool.QueryRow(check, `SELECT status='HEALTHY' AND recovery_state='ACTIVE' AND last_heartbeat>=clock_timestamp()-($2 * interval '1 second') FROM workers WHERE id=$1`, *d.WorkerID, s.HeartbeatTimeout.Seconds()).Scan(&workerHealthy)
			return s.recordError(ctx, d, "cold start readiness timeout", checkErr == nil && workerHealthy)
		}
		elapsed := time.Since(started).Milliseconds()
		result, err := s.Pool.Exec(ctx, `UPDATE deployments SET status='RUNNING',host_port=$4,last_request_at=clock_timestamp(),health_state=NULL,health_checked_at=NULL,idle_error=NULL,cold_start_ms=$5 WHERE id=$1 AND version=$2 AND idle_epoch=$3 AND status='WAKING'`, d.ID, d.Version, d.IdleEpoch, status.HostPort, elapsed)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return errors.New("wake superseded")
		}
		s.Logger.Info("application cold start", "deployment_id", d.ID, "version", d.Version, "latency_ms", elapsed)
	} else {
		if _, err := s.Pool.Exec(ctx, `UPDATE deployments SET status='SLEEPING',host_port=NULL,capacity_reserved=FALSE,health_state=NULL,health_checked_at=NULL,idle_error=NULL WHERE id=$1 AND version=$2 AND idle_epoch=$3 AND status='SUSPENDING'`, d.ID, d.Version, d.IdleEpoch); err != nil {
			return err
		}
		s.Logger.Info("application sleeping", "deployment_id", d.ID)
	}
	s.Invalidate(*d.PublicIdentifier)
	return nil
}

func (s *Service) reserveWake(ctx context.Context, d deployments.Deployment) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(110011)`); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE deployments d SET status='WAKING',capacity_reserved=TRUE,idle_epoch=idle_epoch+1,idle_attempts=0,idle_error=NULL WHERE d.id=$1 AND d.version=$2 AND d.status='SLEEPING' AND EXISTS(SELECT 1 FROM workers w WHERE w.id=d.worker_id AND w.recovery_state='ACTIVE' AND w.status='HEALTHY' AND w.last_heartbeat>=clock_timestamp()-($3 * interval '1 second') AND w.available_cpu-COALESCE((SELECT SUM(reserved_cpu) FROM deployments WHERE worker_id=w.id AND capacity_reserved),0)>=d.reserved_cpu AND w.available_memory-COALESCE((SELECT SUM(reserved_memory) FROM deployments WHERE worker_id=w.id AND capacity_reserved),0)>=d.reserved_memory)`, d.ID, d.Version, s.HeartbeatTimeout.Seconds())
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return errors.New("worker has no wake capacity or deployment changed")
	}
	return tx.Commit(ctx)
}
func (s *Service) recordError(_ context.Context, d deployments.Deployment, message string, terminal bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.Pool.Exec(ctx, `UPDATE deployments SET idle_error=$4,status=CASE WHEN $5 THEN 'FAILED'::deployment_status ELSE status END WHERE id=$1 AND version=$2 AND idle_epoch=$3 AND status IN ('SUSPENDING','WAKING')`, d.ID, d.Version, d.IdleEpoch, message, terminal)
	if err != nil {
		return err
	}
	return errors.New(message)
}
func (s *Service) ready(ctx context.Context, address string, port int, path string) error {
	u, err := url.Parse(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsPrivate() {
		return errors.New("invalid private application address")
	}
	target := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if path == "" {
			conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", target)
			if err == nil {
				conn.Close()
				return nil
			}
		} else {
			req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://%s%s", target, path), nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
