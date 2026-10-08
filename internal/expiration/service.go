package expiration

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/worker"
)

type Repository interface {
	ClaimExpiration(context.Context, int) ([]deployments.Deployment, error)
	MarkExpired(context.Context, string, int) error
	RecordExpirationError(context.Context, string, int, string, time.Duration) error
}

type Service struct {
	Pool       *pgxpool.Pool
	Repository Repository
	Client     func(string) (worker.Client, error)
	Logger     *slog.Logger
	Invalidate func(string)
}

func (s *Service) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.Logger.Error("deployment expiration scan", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) Tick(ctx context.Context) error {
	items, err := s.Repository.ClaimExpiration(ctx, 32)
	if err != nil {
		return err
	}
	for _, d := range items {
		if err := s.expire(ctx, d); err != nil && ctx.Err() == nil {
			delay := 15 * time.Second * time.Duration(1<<min(d.ExpirationAttempts-1, 4))
			if recordErr := s.Repository.RecordExpirationError(ctx, d.ID, d.Version, err.Error(), delay); recordErr != nil {
				s.Logger.Error("record expiration retry", "deployment_id", d.ID, "error", recordErr)
			}
			s.Logger.Warn("deployment cleanup pending", "deployment_id", d.ID, "attempt", d.ExpirationAttempts, "error", err)
		}
	}
	return nil
}

func (s *Service) expire(ctx context.Context, d deployments.Deployment) error {
	if d.WorkerID == nil && d.ContainerID != nil {
		return errors.New("deployment has a container but no registered worker assignment")
	}
	if d.WorkerID != nil {
		var recoveryState string
		err := s.Pool.QueryRow(ctx, `SELECT recovery_state FROM workers WHERE id=$1`, *d.WorkerID).Scan(&recoveryState)
		if err != nil {
			return err
		}
		if recoveryState != "FENCED" {
			if d.WorkerAddress == nil {
				return errors.New("assigned worker address is missing")
			}
			client, err := s.Client(*d.WorkerAddress)
			if err != nil {
				return err
			}
			cleaner, ok := client.(worker.CleanupClient)
			if !ok {
				return errors.New("worker does not support idempotent cleanup")
			}
			cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err = cleaner.Cleanup(cleanupCtx, d.ID, d.Version)
			cancel()
			if err != nil {
				return err
			}
		}
	}
	if err := s.Repository.MarkExpired(ctx, d.ID, d.Version); err != nil {
		return err
	}
	if d.PublicIdentifier != nil {
		s.Invalidate(*d.PublicIdentifier)
	}
	s.Logger.Info("deployment expired", "deployment_id", d.ID, "attempt", d.ExpirationAttempts)
	return nil
}
