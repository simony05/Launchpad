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
	"github.com/simon/launchpad/internal/worker"
	"github.com/simon/launchpad/internal/workers"
	"github.com/simon/launchpad/internal/workspace"
)

func main() {
	cfg, err := config.LoadWorker()
	if err != nil {
		slog.Error("invalid worker configuration", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	heartbeatCfg, err := config.LoadHeartbeat()
	if err != nil {
		logger.Error("invalid heartbeat configuration", "error", err)
		os.Exit(1)
	}
	server := worker.NewServer(cfg.Address(), cfg.WorkerToken, logger, workspace.NewLocalStore(cfg.WorkspaceRoot), build.NewDockerBuilder(cfg.WorkspaceRoot, time.Duration(cfg.BuildTimeoutSeconds)*time.Second), containers.NewDockerManager(time.Duration(cfg.StartTimeoutSeconds)*time.Second))
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("worker stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		worker.ReportHeartbeats(ctx, heartbeatCfg.ControlPlaneURL, cfg.WorkerToken, workers.Report{ID: heartbeatCfg.ID, Hostname: heartbeatCfg.Hostname, Address: heartbeatCfg.Address}, containers.NewDockerManager(5*time.Second), heartbeatCfg.Interval, logger)
	}()
	<-ctx.Done()
	<-heartbeatDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}
