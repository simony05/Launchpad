package deployments

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const deploymentColumns = `
	id::text, name, status::text, runtime, version, created_at, updated_at,
	container_id, internal_port, host_port, public_identifier, image_name, build_log, build_error, start_error`

// PostgresRepository stores deployment metadata in PostgreSQL.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) Create(ctx context.Context, input CreateInput) (Deployment, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO deployments (name, status, runtime, public_identifier)
		VALUES ($1, $2, $3, replace(gen_random_uuid()::text, '-', ''))
		RETURNING `+deploymentColumns, input.Name, StatusPending, input.Runtime)

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
		SET status = $2, container_id = $3, internal_port = $4, host_port = $5, start_error = NULL
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

func (r *PostgresRepository) MarkStopped(ctx context.Context, id string) (Deployment, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE deployments
		SET status = $2, container_id = NULL, internal_port = NULL, host_port = NULL
		WHERE id = $1 AND status NOT IN ($3, $4)
		RETURNING `+deploymentColumns, id, StatusStopped, StatusBuilding, StatusStarting)
	return r.scanUpdatedDeployment(row, "mark deployment stopped")
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
