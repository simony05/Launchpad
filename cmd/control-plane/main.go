package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/simon/launchpad/internal/build"
	"github.com/simon/launchpad/internal/config"
	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/database"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/httpserver"
	"github.com/simon/launchpad/internal/workspace"
)

const shutdownTimeout = 10 * time.Second
const startupTimeout = 10 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	slog.SetDefault(logger)

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), startupTimeout)
	defer cancelStartup()
	pool, err := database.Open(startupCtx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("connect to PostgreSQL", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := database.Migrate(startupCtx, pool); err != nil {
		logger.Error("migrate PostgreSQL", "error", err)
		os.Exit(1)
	}

	deploymentRepository := deployments.NewPostgresRepository(pool)
	sourceStore := workspace.NewLocalStore(cfg.WorkspaceRoot)
	buildTimeout := time.Duration(cfg.BuildTimeoutSeconds) * time.Second
	startTimeout := time.Duration(cfg.StartTimeoutSeconds) * time.Second
	builder := build.NewDockerBuilder(cfg.WorkspaceRoot, buildTimeout)
	containerManager := containers.NewDockerManager(startTimeout)
	server := httpserver.New(cfg.Address(), logger, deploymentRepository, sourceStore, builder, containerManager, containers.Limits{CPUs: cfg.AppCPUs, Memory: cfg.AppMemory}, buildTimeout, startTimeout)

	go func() {
		logger.Info("control plane listening", "address", cfg.Address())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("control plane stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	logger.Info("control plane shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}

	logger.Info("control plane stopped")
}
