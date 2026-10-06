package deployments

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

func ValidateHealthPath(path string) error {
	if path == "" {
		return nil
	}
	u, err := url.ParseRequestURI(path)
	if err != nil || len(path) > 256 || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || u.IsAbs() || u.Host != "" || u.Fragment != "" {
		return errors.New("health_path must be an absolute relative path such as /health (maximum 256 bytes)")
	}
	return nil
}

func (r *PostgresRepository) ListRunningOnWorker(ctx context.Context, id string) ([]Deployment, error) {
	rows, err := r.pool.Query(ctx, `SELECT id::text,version,container_id,health_path,restart_attempts FROM deployments WHERE worker_id=$1 AND status='RUNNING' ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Deployment{}
	for rows.Next() {
		var d Deployment
		if err := rows.Scan(&d.ID, &d.Version, &d.ContainerID, &d.HealthPath, &d.RestartAttempts); err != nil {
			return nil, err
		}
		items = append(items, d)
	}
	return items, rows.Err()
}
