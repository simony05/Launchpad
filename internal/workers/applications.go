package workers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/simon/launchpad/internal/deployments"
)

type Observation struct {
	LogTail          string `json:"log_tail"`
	DeploymentID     string `json:"deployment_id"`
	ContainerID      string `json:"container_id"`
	Version          int    `json:"version"`
	State            string `json:"state"`
	Message          string `json:"message"`
	ExitCode         int    `json:"exit_code"`
	OOMKilled        bool   `json:"oom_killed"`
	ExpectedAttempts int    `json:"expected_attempts"`
	RequestRestart   bool   `json:"request_restart"`
}
type RecoveryDecision struct {
	Restart bool `json:"restart"`
}

type Applications struct {
	Pool        *pgxpool.Pool
	MaxRestarts int
	Cooldown    time.Duration
}

// Observe locks the deployment row so claims, terminal failure and API deletion
// cannot reset counters or apply a report to a replaced container.
func (a Applications) Observe(ctx context.Context, workerID string, o Observation) (RecoveryDecision, error) {
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return RecoveryDecision{}, err
	}
	defer tx.Rollback(ctx)
	var attempts int
	var eligible bool
	err = tx.QueryRow(ctx, `SELECT restart_attempts,(next_restart_at IS NULL OR next_restart_at <= clock_timestamp()) FROM deployments
 WHERE id=$1 AND worker_id=$2 AND container_id=$3 AND version=$4 AND status='RUNNING'
 AND EXISTS(SELECT 1 FROM workers WHERE id=$2 AND recovery_state='ACTIVE') FOR UPDATE`, o.DeploymentID, workerID, o.ContainerID, o.Version).Scan(&attempts, &eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryDecision{}, nil
	}
	if err != nil {
		return RecoveryDecision{}, err
	}
	if attempts != o.ExpectedAttempts {
		return RecoveryDecision{}, nil
	}
	failed := o.State == "EXITED" && attempts >= a.MaxRestarts
	decision := RecoveryDecision{Restart: o.RequestRestart && o.State == "EXITED" && !failed && eligible}
	message := o.Message
	if failed {
		message = "restart limit exhausted: " + message
	}
	_, err = tx.Exec(ctx, `UPDATE deployments SET health_state=$2,health_checked_at=clock_timestamp(),
 runtime_error=NULLIF($3,''),last_failure=CASE WHEN $2='RUNNING' THEN last_failure ELSE $3 END,
 last_exit_code=CASE WHEN $2='EXITED' THEN $4 ELSE last_exit_code END,
 oom_killed=CASE WHEN $2='EXITED' THEN $5 ELSE oom_killed END,
 restart_attempts=restart_attempts+CASE WHEN $6 THEN 1 ELSE 0 END,
 next_restart_at=CASE WHEN $6 THEN clock_timestamp()+($7 * interval '1 second') ELSE next_restart_at END,
	 status=CASE WHEN $8 THEN 'FAILED'::deployment_status ELSE status END,
	 runtime_logs=CASE WHEN $9<>'' THEN $9 ELSE runtime_logs END
 WHERE id=$1`, o.DeploymentID, o.State, message, o.ExitCode, o.OOMKilled, decision.Restart, a.Cooldown.Seconds(), failed, o.LogTail)
	if err != nil {
		return RecoveryDecision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RecoveryDecision{}, err
	}
	return decision, nil
}

func (a Applications) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /internal/workers/{id}/idle/{deployment}", func(w http.ResponseWriter, r *http.Request) {
		state := "SUSPENDING"
		if r.URL.Query().Get("action") == "wake" {
			state = "WAKING"
		} else if r.URL.Query().Get("action") != "sleep" {
			http.Error(w, "invalid action", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var allowed bool
		err := a.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployments d JOIN workers w ON w.id=d.worker_id WHERE d.id::text=$1 AND w.id::text=$2 AND d.version::text=$3 AND d.idle_epoch::text=$4 AND d.status::text=$5 AND w.recovery_state='ACTIVE')`, r.PathValue("deployment"), r.PathValue("id"), r.URL.Query().Get("version"), r.URL.Query().Get("epoch"), state).Scan(&allowed)
		if err != nil {
			http.Error(w, "lookup failed", 503)
			return
		}
		if !allowed {
			http.Error(w, "operation revoked", 409)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /internal/workers/{id}/assignments/{deployment}", func(w http.ResponseWriter, r *http.Request) {
		if _, err := uuid.Parse(r.PathValue("deployment")); err != nil {
			http.Error(w, "invalid deployment id", 400)
			return
		}
		if _, err := uuid.Parse(r.PathValue("id")); err != nil {
			http.Error(w, "invalid worker id", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var allowed bool
		err := a.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployments d JOIN workers w ON w.id=d.worker_id WHERE d.id=$1 AND w.id=$2 AND d.version::text=$3 AND d.status IN ('BUILDING','READY_TO_START','STARTING','RECOVERING','RUNNING') AND w.recovery_state='ACTIVE')`, r.PathValue("deployment"), r.PathValue("id"), r.URL.Query().Get("version")).Scan(&allowed)
		if err != nil {
			http.Error(w, "assignment lookup failed", 503)
			return
		}
		if !allowed {
			http.Error(w, "assignment revoked", 409)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /internal/workers/{id}/applications", func(w http.ResponseWriter, r *http.Request) {
		if _, err := uuid.Parse(r.PathValue("id")); err != nil {
			http.Error(w, "invalid worker id", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		items, err := deployments.NewPostgresRepository(a.Pool).ListRunningOnWorker(ctx, r.PathValue("id"))
		if err != nil {
			http.Error(w, "list applications failed", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
	})
	mux.HandleFunc("POST /internal/workers/{id}/applications", func(w http.ResponseWriter, r *http.Request) {
		var o Observation
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 48<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&o); err != nil {
			http.Error(w, "invalid observation", 400)
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			http.Error(w, "expected one observation", 400)
			return
		}
		_, idErr := uuid.Parse(o.DeploymentID)
		_, workerErr := uuid.Parse(r.PathValue("id"))
		if idErr != nil || workerErr != nil || o.Version < 1 || o.ExpectedAttempts < 0 || o.ContainerID == "" || len(o.ContainerID) > 64 || len(o.Message) > 2048 || len(o.LogTail) > 4096 || (o.State != "RUNNING" && o.State != "UNHEALTHY" && o.State != "EXITED") {
			http.Error(w, "invalid observation fields", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		result, err := a.Observe(ctx, r.PathValue("id"), o)
		if err != nil {
			http.Error(w, "record observation failed", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
}
