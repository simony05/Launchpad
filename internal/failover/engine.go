package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/simon/launchpad/internal/build"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/scheduler"
	"github.com/simon/launchpad/internal/worker"
	"github.com/simon/launchpad/internal/workspace"
)

type Engine struct {
	Pool                                      *pgxpool.Pool
	Fencer                                    Fencer
	Client                                    func(string) (worker.Client, error)
	Grace, HeartbeatTimeout, OperationTimeout time.Duration
	MaxAttempts                               int
	Logger                                    *slog.Logger
	Invalidate                                func(string)
}

func (e *Engine) Run(ctx context.Context, interval time.Duration) {
	// Let workers re-register after a control-plane/DB outage before declaring loss.
	timer := time.NewTimer(e.Grace)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := e.Tick(ctx); err != nil && ctx.Err() == nil {
			e.Logger.Error("worker failover scan", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (e *Engine) Tick(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, e.OperationTimeout+30*time.Second)
	defer cancel()
	// A session lock serializes reconcilers, including RPCs. Connection loss releases
	// it; durable generations and deterministic worker starts handle replay.
	conn, err := e.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(140014)`).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(cleanup, `SELECT pg_advisory_unlock(140014)`); err != nil {
			_ = conn.Conn().Close(cleanup)
		}
	}()
	if err := e.fence(ctx); err != nil {
		e.Logger.Warn("worker fencing pending", "error", err)
	}
	job, err := e.prepare(ctx)
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, scheduler.ErrNoCapacity) {
		return nil
	}
	if err != nil {
		return err
	}
	return e.execute(ctx, job)
}

func (e *Engine) fence(ctx context.Context) error {
	var target Target
	// Atomic heartbeat race: either a recent heartbeat wins or quarantine wins.
	err := e.Pool.QueryRow(ctx, `UPDATE workers SET recovery_state='FENCING',status='UNHEALTHY',fencing_attempted_at=clock_timestamp()
 WHERE id=(SELECT id FROM workers WHERE recovery_state='FENCING' OR
 (recovery_state='ACTIVE' AND instance_id<>'' AND last_heartbeat<clock_timestamp()-($1 * interval '1 second'))
 ORDER BY fencing_attempted_at NULLS FIRST,last_heartbeat LIMIT 1)
 AND (recovery_state='FENCING' OR (recovery_state='ACTIVE' AND last_heartbeat<clock_timestamp()-($1 * interval '1 second')))
 RETURNING id::text,instance_id,address`, e.Grace.Seconds()).Scan(&target.ID, &target.InstanceID, &target.Address)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	fenceCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := e.Fencer.Fence(fenceCtx, target); err != nil {
		_, _ = e.Pool.Exec(ctx, `UPDATE workers SET fencing_error=$2 WHERE id=$1`, target.ID, err.Error())
		return err
	}
	_, err = e.Pool.Exec(ctx, `UPDATE workers SET recovery_state='FENCED',fenced_at=clock_timestamp(),fencing_error=NULL WHERE id=$1`, target.ID)
	return err
}

type Job struct {
	ID, WorkerID, Address, PublicIdentifier string
	Version, Retries                        int
	Files                                   workspace.Files
	Limits                                  containers.Limits
}

func (e *Engine) prepare(ctx context.Context) (Job, error) {
	tx, err := e.Pool.Begin(ctx)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(110011)`); err != nil {
		return Job{}, err
	}
	var j Job
	var source []byte
	var cpu float64
	var memory int64
	var attempts int
	var workerState string
	err = tx.QueryRow(ctx, `SELECT d.id::text,d.worker_id::text,d.worker_address,d.version,COALESCE(d.public_identifier,''),d.source_files,d.reserved_cpu,d.reserved_memory,d.failover_attempts,d.recovery_retries,w.recovery_state
 FROM deployments d JOIN workers w ON w.id=d.worker_id
 WHERE (w.recovery_state='FENCED' AND d.status IN ('RUNNING','BUILDING','READY_TO_START','STARTING','RECOVERING','WAKING'))
 OR (d.status='RECOVERING' AND w.recovery_state='ACTIVE' AND w.status='HEALTHY' AND w.last_heartbeat>=clock_timestamp()-($1 * interval '1 second'))
 ORDER BY d.updated_at,d.id LIMIT 1 FOR UPDATE OF d`, e.HeartbeatTimeout.Seconds()).Scan(&j.ID, &j.WorkerID, &j.Address, &j.Version, &j.PublicIdentifier, &source, &cpu, &memory, &attempts, &j.Retries, &workerState)
	if err != nil {
		return j, err
	}
	reason := ""
	if source == nil {
		reason = "source unavailable: redeploy this pre-milestone-14 application"
	} else if err := json.Unmarshal(source, &j.Files); err != nil {
		reason = "stored source is invalid"
	} else if err := workspace.ValidateFiles(j.Files); err != nil {
		reason = "stored source validation failed"
	}
	if workerState == "FENCED" && attempts >= e.MaxAttempts {
		reason = "worker failover attempt limit exhausted"
	}
	if cpu <= 0 || memory <= 0 {
		reason = "original resource reservation unavailable"
	}
	if reason != "" {
		_, err = tx.Exec(ctx, `UPDATE deployments SET status='FAILED',recovery_error=$2 WHERE id=$1`, j.ID, reason)
		if err != nil {
			return j, err
		}
		if err := tx.Commit(ctx); err != nil {
			return j, err
		}
		e.Invalidate(j.PublicIdentifier)
		return j, pgx.ErrNoRows
	}
	j.Limits = containers.Limits{CPUs: strconv.FormatFloat(cpu, 'f', -1, 64), Memory: fmt.Sprintf("%dm", memory/(1<<20))}
	if workerState == "FENCED" {
		a, err := scheduler.Choose(ctx, tx, e.HeartbeatTimeout, cpu, memory, j.WorkerID)
		if errors.Is(err, scheduler.ErrNoCapacity) {
			_, updateErr := tx.Exec(ctx, `UPDATE deployments SET status='RECOVERING',recovery_error=$2 WHERE id=$1`, j.ID, err.Error())
			if updateErr != nil {
				return j, updateErr
			}
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return j, commitErr
			}
			e.Invalidate(j.PublicIdentifier)
			return j, err
		}
		if err != nil {
			return j, err
		}
		j.WorkerID = a.WorkerID
		j.Address = a.Address
		j.Version++
		j.Retries = 0
		_, err = tx.Exec(ctx, `UPDATE deployments SET status='RECOVERING',worker_id=$2,worker_address=$3,version=$4,container_id=NULL,host_port=NULL,internal_port=NULL,capacity_reserved=TRUE,
 failover_attempts=failover_attempts+1,recovery_retries=0,recovery_started_at=clock_timestamp(),recovery_error=NULL,health_state=NULL,health_checked_at=NULL WHERE id=$1`, j.ID, j.WorkerID, j.Address, j.Version)
		if err != nil {
			return j, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return j, err
	}
	e.Invalidate(j.PublicIdentifier)
	return j, nil
}

func (e *Engine) execute(ctx context.Context, j Job) error {
	client, err := e.Client(j.Address)
	if err != nil {
		return e.failure(ctx, j, err.Error(), false)
	}
	// First reconcile an uncertain prior RPC instead of issuing another build.
	if j.Retries > 0 {
		status, err := client.Status(ctx, j.ID, j.Version)
		if err == nil && status.DeploymentID == j.ID && status.Version == j.Version && status.Running && status.ContainerID != "" && status.HostPort > 0 {
			return e.finish(ctx, j, worker.StartResult{ImageName: build.ImageName(j.ID, j.Version), Container: &containers.Container{ID: status.ContainerID, InternalPort: 8000, HostPort: status.HostPort}})
		}
	}
	if j.Retries >= 3 {
		return e.failure(ctx, j, "recovery RPC retry limit exhausted; inspect assigned worker before releasing capacity", true)
	}
	result, err := e.Pool.Exec(ctx, `UPDATE deployments SET recovery_retries=recovery_retries+1 WHERE id=$1 AND version=$2 AND status='RECOVERING'`, j.ID, j.Version)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return nil
	}
	opCtx, cancel := context.WithTimeout(ctx, e.OperationTimeout)
	defer cancel()
	started, err := client.Start(opCtx, worker.StartRequest{DeploymentID: j.ID, Version: j.Version, Files: j.Files, Limits: j.Limits})
	if err != nil {
		return e.failure(ctx, j, err.Error(), false)
	}
	if started.BuildError != "" || started.StartError != "" || started.Container == nil {
		if _, err := e.Pool.Exec(ctx, `UPDATE deployments SET build_log=$3,build_error=NULLIF($4,''),start_error=NULLIF($5,'') WHERE id=$1 AND version=$2 AND status='RECOVERING'`, j.ID, j.Version, started.BuildLog, started.BuildError, started.StartError); err != nil {
			return err
		}
		return e.failure(ctx, j, "recovery build/start failed: "+started.BuildError+" "+started.StartError, true)
	}
	return e.finish(ctx, j, started)
}
func (e *Engine) failure(ctx context.Context, j Job, message string, terminal bool) error {
	_, err := e.Pool.Exec(ctx, `UPDATE deployments SET recovery_error=$3,status=CASE WHEN $4 THEN 'FAILED'::deployment_status ELSE status END WHERE id=$1 AND version=$2 AND status='RECOVERING'`, j.ID, j.Version, message, terminal)
	return err
}
func (e *Engine) finish(ctx context.Context, j Job, r worker.StartResult) error {
	if r.Container == nil || !regexp.MustCompile(`^[a-f0-9]{12,64}$`).MatchString(r.Container.ID) || r.Container.InternalPort != 8000 || r.Container.HostPort < 1 || r.Container.HostPort > 65535 {
		return e.failure(ctx, j, "worker returned invalid recovery location", true)
	}
	_, err := e.Pool.Exec(ctx, `UPDATE deployments SET status='RUNNING',container_id=$3,host_port=$4,internal_port=$5,image_name=$6,build_log=$7,recovery_error=NULL,health_state=NULL,health_checked_at=NULL,runtime_error=NULL,last_request_at=clock_timestamp() WHERE id=$1 AND version=$2 AND worker_id=$8 AND status='RECOVERING'`, j.ID, j.Version, r.Container.ID, r.Container.HostPort, r.Container.InternalPort, r.ImageName, r.BuildLog, j.WorkerID)
	e.Invalidate(j.PublicIdentifier)
	return err
}
