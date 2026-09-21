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
	id::text, name, status::text, runtime, created_at, updated_at,
	container_id, internal_port, public_identifier`

// PostgresRepository stores deployment metadata in PostgreSQL.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) Create(ctx context.Context, input CreateInput) (Deployment, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO deployments (name, status, runtime)
		VALUES ($1, $2, $3)
		RETURNING `+deploymentColumns, input.Name, StatusPending, input.Runtime)

	deployment, err := scanDeployment(row)
	if err != nil {
		return Deployment{}, fmt.Errorf("create deployment: %w", err)
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

type rowScanner interface {
	Scan(...any) error
}

func scanDeployment(row rowScanner) (Deployment, error) {
	var deployment Deployment
	var containerID, publicIdentifier pgtype.Text
	var internalPort pgtype.Int4

	err := row.Scan(
		&deployment.ID,
		&deployment.Name,
		&deployment.Status,
		&deployment.Runtime,
		&deployment.CreatedAt,
		&deployment.UpdatedAt,
		&containerID,
		&internalPort,
		&publicIdentifier,
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
	if publicIdentifier.Valid {
		deployment.PublicIdentifier = &publicIdentifier.String
	}

	return deployment, nil
}
