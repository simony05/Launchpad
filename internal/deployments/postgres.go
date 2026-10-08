package deployments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const deploymentColumns = `
	id::text, name, status::text, runtime, version, created_at, updated_at,
	container_id, internal_port, host_port, public_identifier, image_name, build_log, build_error, start_error, worker_id::text, worker_address,
	health_path, health_state, health_checked_at, restart_attempts, next_restart_at, last_exit_code, oom_killed, runtime_error, last_failure, runtime_logs,failover_attempts,recovery_error,last_request_at,idle_epoch,idle_error,cold_start_ms,ttl_seconds,expires_at,expiration_attempts,expiration_error`

// PostgresRepository stores deployment metadata in PostgreSQL.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) Create(ctx context.Context, input CreateInput) (Deployment, error) {
	var source []byte
	if input.Files != nil {
		var err error
		source, err = json.Marshal(input.Files)
		if err != nil {
			return Deployment{}, err
		}
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO deployments (name, status, runtime, public_identifier,health_path,source_files,ttl_seconds,expires_at)
		VALUES ($1, $2, $3, replace(gen_random_uuid()::text, '-', ''),$4,$5,$6,CASE WHEN $6=0 THEN NULL ELSE clock_timestamp()+($6 * interval '1 second') END)
		RETURNING `+deploymentColumns, input.Name, StatusPending, input.Runtime, input.HealthPath, source, input.TTLSeconds)

	deployment, err := scanDeployment(row)
	if err != nil {
		return Deployment{}, fmt.Errorf("create deployment: %w", err)
	}
	return deployment, nil
}

func (r *PostgresRepository) GetByPublicIdentifier(ctx context.Context, publicIdentifier string) (Deployment, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE public_identifier = $1`, publicIdentifier)
	deployment, err := scanDeployment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("get deployment by public identifier: %w", err)
	}
	return deployment, nil
}

func (r *PostgresRepository) Get(ctx context.Context, id string) (Deployment, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE id = $1`, id)
	deployment, err := scanDeployment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("get deployment: %w", err)
	}
	return deployment, nil
}

func (r *PostgresRepository) List(ctx context.Context) ([]Deployment, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+deploymentColumns+` FROM deployments ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()

	deployments := make([]Deployment, 0)
	for rows.Next() {
		deployment, err := scanDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan deployment: %w", err)
		}
		deployments = append(deployments, deployment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deployments: %w", err)
	}

	return deployments, nil
}

func (r *PostgresRepository) Delete(ctx context.Context, id string) error {
	commandTag, err := r.pool.Exec(ctx, "DELETE FROM deployments WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("delete deployment: %w", err)
	}
	if commandTag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) MarkBuilding(ctx context.Context, id string) (Deployment, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE deployments
		SET status = $2, image_name = NULL, build_log = NULL, build_error = NULL
		WHERE id = $1 AND status = $3
		RETURNING `+deploymentColumns, id, StatusBuilding, StatusPending)
	return r.scanUpdatedDeployment(row, "mark deployment building")
}

func (r *PostgresRepository) CompleteBuild(ctx context.Context, id, imageName, buildLog string) (Deployment, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE deployments
		SET status = $2, image_name = $3, build_log = $4, build_error = NULL
		WHERE id = $1 AND status = $5
		RETURNING `+deploymentColumns, id, StatusReadyToStart, imageName, buildLog, StatusBuilding)
	return r.scanUpdatedDeployment(row, "complete deployment build")
}

func (r *PostgresRepository) FailBuild(ctx context.Context, id, buildLog, buildError string) (Deployment, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE deployments
		SET status = $2, build_log = $3, build_error = $4
		WHERE id = $1 AND status = $5
		RETURNING `+deploymentColumns, id, StatusFailed, buildLog, buildError, StatusBuilding)
	return r.scanUpdatedDeployment(row, "fail deployment build")
}

func (r *PostgresRepository) MarkStarting(ctx context.Context, id string) (Deployment, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE deployments
		SET status = $2, container_id = NULL, internal_port = NULL, host_port = NULL, start_error = NULL
		WHERE id = $1 AND status = $3
		RETURNING `+deploymentColumns, id, StatusStarting, StatusReadyToStart)
	return r.scanUpdatedDeployment(row, "mark deployment starting")
}

func (r *PostgresRepository) CompleteStart(ctx context.Context, id, containerID string, internalPort, hostPort int) (Deployment, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE deployments
		SET status = $2, container_id = $3, internal_port = $4, host_port = $5, start_error = NULL,last_request_at=clock_timestamp()
		WHERE id = $1 AND status = $6
		RETURNING `+deploymentColumns, id, StatusRunning, containerID, internalPort, hostPort, StatusStarting)
	return r.scanUpdatedDeployment(row, "complete deployment start")
}

func (r *PostgresRepository) FailStart(ctx context.Context, id, startError string) (Deployment, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE deployments
		SET status = $2, start_error = $3
		WHERE id = $1 AND status = $4
		RETURNING `+deploymentColumns, id, StatusFailed, startError, StatusStarting)
	return r.scanUpdatedDeployment(row, "fail deployment start")
}

func (r *PostgresRepository) MarkStopped(ctx context.Context, id string, version int) (Deployment, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE deployments
		SET status = $2, container_id = NULL, internal_port = NULL, host_port = NULL
		WHERE id = $1 AND status NOT IN ($3, $4,'RECOVERING','SUSPENDING','WAKING','EXPIRING','EXPIRED') AND version=$5
		RETURNING `+deploymentColumns, id, StatusStopped, StatusBuilding, StatusStarting, version)
	return r.scanUpdatedDeployment(row, "mark deployment stopped")
}

// ClaimStop revokes idle/start operations before contacting the worker.
func (r *PostgresRepository) ClaimStop(ctx context.Context, id string, version int) (Deployment, error) {
	return r.scanUpdatedDeployment(r.pool.QueryRow(ctx, `UPDATE deployments SET status='STOPPING' WHERE id=$1 AND version=$2 AND status NOT IN ('BUILDING','READY_TO_START','STARTING','RECOVERING','SUSPENDING','WAKING','EXPIRING','EXPIRED') RETURNING `+deploymentColumns, id, version), "claim deployment stop")
}

// ClaimExpiration makes the URL unavailable before worker cleanup starts. The row
// lock lets several control-plane processes safely share the expiration scan.
func (r *PostgresRepository) ClaimExpiration(ctx context.Context, limit int) ([]Deployment, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE (expires_at<=clock_timestamp() AND status NOT IN ('EXPIRED','EXPIRING','STOPPING')) OR (status='EXPIRING' AND (expiration_retry_at IS NULL OR expiration_retry_at<=clock_timestamp())) ORDER BY expires_at NULLS FIRST,id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		if _, err := tx.Exec(ctx, `UPDATE deployments SET status='EXPIRING',expiration_attempts=expiration_attempts+1,expiration_retry_at=clock_timestamp()+interval '30 seconds' WHERE id=$1`, out[i].ID); err != nil {
			return nil, err
		}
		out[i].Status = StatusExpiring
		out[i].ExpirationAttempts++
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
func (r *PostgresRepository) MarkExpired(ctx context.Context, id string, version int) error {
	res, err := r.pool.Exec(ctx, `UPDATE deployments SET status='EXPIRED',container_id=NULL,internal_port=NULL,host_port=NULL,image_name=NULL,build_log=NULL,build_error=NULL,start_error=NULL,runtime_error=NULL,runtime_logs='',last_failure=NULL,recovery_error=NULL,idle_error=NULL,source_files=NULL,capacity_reserved=FALSE,expiration_error=NULL,expiration_retry_at=NULL,health_state=NULL WHERE id=$1 AND version=$2 AND status='EXPIRING'`, id, version)
	if err == nil && res.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return err
}
func (r *PostgresRepository) RecordExpirationError(ctx context.Context, id string, version int, message string, delay time.Duration) error {
	_, err := r.pool.Exec(ctx, `UPDATE deployments SET expiration_error=$3,expiration_retry_at=clock_timestamp()+($4 * interval '1 second') WHERE id=$1 AND version=$2 AND status='EXPIRING'`, id, version, message, delay.Seconds())
	return err
}

func (r *PostgresRepository) scanUpdatedDeployment(row pgx.Row, operation string) (Deployment, error) {
	deployment, err := scanDeployment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrInvalidState
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("%s: %w", operation, err)
	}
	return deployment, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanDeployment(row rowScanner) (Deployment, error) {
	var deployment Deployment
	var containerID, publicIdentifier, imageName, buildLog, buildError, startError pgtype.Text
	var internalPort, hostPort pgtype.Int4

	err := row.Scan(
		&deployment.ID,
		&deployment.Name,
		&deployment.Status,
		&deployment.Runtime,
		&deployment.Version,
		&deployment.CreatedAt,
		&deployment.UpdatedAt,
		&containerID,
		&internalPort,
		&hostPort,
		&publicIdentifier,
		&imageName,
		&buildLog,
		&buildError,
		&startError,
		&deployment.WorkerID,
		&deployment.WorkerAddress,
		&deployment.HealthPath, &deployment.HealthState, &deployment.HealthCheckedAt, &deployment.RestartAttempts, &deployment.NextRestartAt, &deployment.LastExitCode, &deployment.OOMKilled, &deployment.RuntimeError, &deployment.LastFailure,
		&deployment.RuntimeLogs,
		&deployment.FailoverAttempts, &deployment.RecoveryError,
		&deployment.LastRequestAt, &deployment.IdleEpoch, &deployment.IdleError, &deployment.ColdStartMS,
		&deployment.TTLSeconds, &deployment.ExpiresAt, &deployment.ExpirationAttempts, &deployment.ExpirationError,
	)
	if err != nil {
		return Deployment{}, err
	}
	if containerID.Valid {
		deployment.ContainerID = &containerID.String
	}
	if internalPort.Valid {
		port := int(internalPort.Int32)
		deployment.InternalPort = &port
	}
	if hostPort.Valid {
		port := int(hostPort.Int32)
		deployment.HostPort = &port
	}
	if publicIdentifier.Valid {
		deployment.PublicIdentifier = &publicIdentifier.String
	}
	if imageName.Valid {
		deployment.ImageName = &imageName.String
	}
	if buildLog.Valid {
		deployment.BuildLog = &buildLog.String
	}
	if buildError.Valid {
		deployment.BuildError = &buildError.String
	}
	if startError.Valid {
		deployment.StartError = &startError.String
	}

	return deployment, nil
}
