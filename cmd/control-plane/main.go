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

	"github.com/simon/launchpad/internal/config"
	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/database"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/httpserver"
	"github.com/simon/launchpad/internal/routing"
	"github.com/simon/launchpad/internal/scheduler"
	"github.com/simon/launchpad/internal/worker"
	"github.com/simon/launchpad/internal/workers"
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
	registryCfg, err := config.LoadRegistry()
	if err != nil {
		logger.Error("invalid registry configuration", "error", err)
		os.Exit(1)
	}

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
	buildTimeout := time.Duration(cfg.BuildTimeoutSeconds) * time.Second
	startTimeout := time.Duration(cfg.StartTimeoutSeconds) * time.Second
	var workerClient worker.Client
	if cfg.WorkerURL != "" {
		workerClient, err = worker.NewHTTPClient(cfg.WorkerURL, cfg.WorkerToken, buildTimeout+startTimeout)
	}
	if err != nil {
		logger.Error("configure worker client", "error", err)
		os.Exit(1)
	}
	verifiedResolver := routing.VerifiedResolver{Deployments: deploymentRepository, Workers: workers.Postgres{Pool: pool}, HeartbeatTimeout: registryCfg.Timeout, LegacyAddress: cfg.WorkerURL, Client: func(address string) (worker.Client, error) {
		return worker.NewHTTPClient(address, cfg.WorkerToken, 3*time.Second)
	}}
	applicationRouter := routing.New(verifiedResolver, cfg.RouterUpstreamHost, cfg.PublicBaseDomain, time.Duration(cfg.RouterCacheTTLSeconds)*time.Second)
	placement := httpserver.Placement{Scheduler: scheduler.Postgres{Pool: pool, HeartbeatTimeout: registryCfg.Timeout}, Client: func(address string) (worker.Client, error) {
		return worker.NewHTTPClient(address, cfg.WorkerToken, buildTimeout+startTimeout)
	}}
	server := httpserver.New(cfg.Address(), logger, deploymentRepository, workerClient, containers.Limits{CPUs: cfg.AppCPUs, Memory: cfg.AppMemory}, applicationRouter, cfg.PublicBaseDomain, buildTimeout, startTimeout, placement)

	go func() {
		logger.Info("control plane listening", "address", cfg.Address())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("control plane stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	registryStore := workers.Postgres{Pool: pool}
	registryServer := &http.Server{Addr: registryCfg.Address, Handler: workers.Handler(registryStore, cfg.WorkerToken), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		logger.Info("worker registry listening", "address", registryCfg.Address)
		if err := registryServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("worker registry failed", "error", err)
			stop()
		}
	}()
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		workers.Monitor(ctx, registryStore, registryCfg.CheckInterval, registryCfg.Timeout, logger)
	}()
	<-ctx.Done()
	<-monitorDone

	logger.Info("control plane shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := registryServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("registry shutdown failed", "error", err)
	}
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}

	logger.Info("control plane stopped")
}
