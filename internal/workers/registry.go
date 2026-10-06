package workers

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Report struct {
	ID                string  `json:"id"`
	Hostname          string  `json:"hostname"`
	Address           string  `json:"address"`
	Healthy           bool    `json:"healthy"`
	TotalCPU          float64 `json:"total_cpu"`
	AvailableCPU      float64 `json:"available_cpu"`
	TotalMemory       int64   `json:"total_memory"`
	AvailableMemory   int64   `json:"available_memory"`
	RunningContainers int     `json:"running_containers"`
}

type Worker struct {
	Report
	Status        string    `json:"status"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

func (r Report) Validate() error {
	id, err := uuid.Parse(r.ID)
	u, urlErr := url.Parse(r.Address)
	if err != nil || id == uuid.Nil || r.Hostname == "" || len(r.Hostname) > 255 || urlErr != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("valid worker UUID, hostname and HTTP address required")
	}
	if math.IsNaN(r.TotalCPU) || math.IsInf(r.TotalCPU, 0) || math.IsNaN(r.AvailableCPU) || math.IsInf(r.AvailableCPU, 0) || r.TotalCPU < 0 || r.AvailableCPU < 0 || r.AvailableCPU > r.TotalCPU || r.TotalMemory < 0 || r.AvailableMemory < 0 || r.AvailableMemory > r.TotalMemory || r.RunningContainers < 0 || (r.Healthy && (r.TotalCPU == 0 || r.TotalMemory == 0)) {
		return errors.New("invalid worker capacity")
	}
	return nil
}

type Store interface {
	Record(context.Context, Report) error
	List(context.Context) ([]Worker, error)
	Expire(context.Context, time.Duration) error
}
type Postgres struct{ Pool *pgxpool.Pool }

func (p Postgres) Get(ctx context.Context, id string) (Worker, error) {
	var w Worker
	err := p.Pool.QueryRow(ctx, `SELECT id::text,hostname,address,status,last_heartbeat FROM workers WHERE id=$1`, id).Scan(&w.ID, &w.Hostname, &w.Address, &w.Status, &w.LastHeartbeat)
	w.Healthy = w.Status == "HEALTHY"
	return w, err
}

// Registration and heartbeat share an atomic upsert, making retries and restarts safe.
func (p Postgres) Record(ctx context.Context, r Report) error {
	status := "UNHEALTHY"
	if r.Healthy {
		status = "HEALTHY"
	}
	_, err := p.Pool.Exec(ctx, `INSERT INTO workers (id,hostname,address,status,total_cpu,available_cpu,total_memory,available_memory,running_containers,last_heartbeat)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,clock_timestamp())
 ON CONFLICT (id) DO UPDATE SET hostname=EXCLUDED.hostname,address=EXCLUDED.address,status=EXCLUDED.status,total_cpu=EXCLUDED.total_cpu,available_cpu=EXCLUDED.available_cpu,total_memory=EXCLUDED.total_memory,available_memory=EXCLUDED.available_memory,running_containers=EXCLUDED.running_containers,last_heartbeat=clock_timestamp()`, r.ID, r.Hostname, r.Address, status, r.TotalCPU, r.AvailableCPU, r.TotalMemory, r.AvailableMemory, r.RunningContainers)
	return err
}
func (p Postgres) Expire(ctx context.Context, timeout time.Duration) error {
	_, err := p.Pool.Exec(ctx, `UPDATE workers SET status='UNHEALTHY' WHERE status='HEALTHY' AND last_heartbeat < clock_timestamp() - ($1 * interval '1 second')`, timeout.Seconds())
	return err
}
func (p Postgres) List(ctx context.Context) ([]Worker, error) {
	rows, err := p.Pool.Query(ctx, `SELECT id::text,hostname,address,status,total_cpu,available_cpu,total_memory,available_memory,running_containers,last_heartbeat FROM workers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Worker{}
	for rows.Next() {
		var w Worker
		if err := rows.Scan(&w.ID, &w.Hostname, &w.Address, &w.Status, &w.TotalCPU, &w.AvailableCPU, &w.TotalMemory, &w.AvailableMemory, &w.RunningContainers, &w.LastHeartbeat); err != nil {
			return nil, err
		}
		w.Healthy = w.Status == "HEALTHY"
		result = append(result, w)
	}
	return result, rows.Err()
}

func Handler(store Store, token string, applications ...Applications) http.Handler {
	mux := http.NewServeMux()
	if len(applications) > 0 {
		applications[0].routes(mux)
	}
	record := func(w http.ResponseWriter, r *http.Request) {
		var report Report
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&report); err != nil {
			http.Error(w, "invalid report", 400)
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			http.Error(w, "expected one report", 400)
			return
		}
		if err := report.Validate(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := store.Record(ctx, report); err != nil {
			http.Error(w, "record worker failed", 503)
			return
		}
		w.WriteHeader(204)
	}
	mux.HandleFunc("POST /internal/workers/register", record)
	mux.HandleFunc("POST /internal/workers/heartbeat", record)
	mux.HandleFunc("GET /internal/workers", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		items, err := store.List(ctx)
		if err != nil {
			http.Error(w, "list workers failed", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func Monitor(ctx context.Context, store Store, interval, timeout time.Duration, logger *slog.Logger) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := store.Expire(checkCtx, timeout)
		cancel()
		if err != nil && ctx.Err() == nil {
			logger.Error("expire worker heartbeats", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
