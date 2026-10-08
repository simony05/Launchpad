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

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/simon/launchpad/internal/config"
	"github.com/simon/launchpad/internal/containers"
	"github.com/simon/launchpad/internal/database"
	"github.com/simon/launchpad/internal/deployments"
	"github.com/simon/launchpad/internal/expiration"
	"github.com/simon/launchpad/internal/failover"
	"github.com/simon/launchpad/internal/httpserver"
	"github.com/simon/launchpad/internal/idle"
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
	recoveryCfg, err := config.LoadRecovery()
	if err != nil {
		logger.Error("invalid recovery configuration", "error", err)
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	idleCfg, err := config.LoadIdle()
	if err != nil {
		logger.Error("invalid idle configuration", "error", err)
		os.Exit(1)
	}
	ttlCfg, err := config.LoadTTL()
	if err != nil {
		logger.Error("invalid TTL configuration", "error", err)
		os.Exit(1)
	}
	failoverCfg, err := config.LoadFailover(registryCfg.Timeout)
	if err != nil {
		logger.Error("invalid failover configuration", "error", err)
		os.Exit(1)
	}
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
	placement := httpserver.Placement{Scheduler: scheduler.Postgres{Pool: pool, HeartbeatTimeout: registryCfg.Timeout}, DefaultTTLSeconds: ttlCfg.DefaultSeconds, Client: func(address string) (worker.Client, error) {
		return worker.NewHTTPClient(address, cfg.WorkerToken, buildTimeout+startTimeout)
	}}
	server := httpserver.New(cfg.Address(), logger, deploymentRepository, workerClient, containers.Limits{CPUs: cfg.AppCPUs, Memory: cfg.AppMemory}, applicationRouter, cfg.PublicBaseDomain, buildTimeout, startTimeout, placement)
	expirationDone := make(chan struct{})
	expirer := &expiration.Service{Repository: deploymentRepository, Client: placement.Client, Logger: logger, Invalidate: applicationRouter.Invalidate, Pool: pool}
	go func() { defer close(expirationDone); expirer.Run(ctx, ttlCfg.CheckInterval) }()
	idleDone := make(chan struct{})
	if idleCfg.Enabled {
		// One router owns in-flight request accounting; do not permit two owners.
		owner, err := pool.Acquire(startupCtx)
		if err != nil {
			logger.Error("acquire idle owner", "error", err)
			os.Exit(1)
		}
		var acquired bool
		if err := owner.QueryRow(startupCtx, `SELECT pg_try_advisory_lock(150015)`).Scan(&acquired); err != nil || !acquired {
			logger.Error("another scale-to-zero router owns this database", "error", err)
			os.Exit(1)
		}
		service := &idle.Service{Pool: pool, Repository: deploymentRepository, Context: ctx, IdleTimeout: idleCfg.Timeout, OperationTimeout: idleCfg.WakeTimeout, HeartbeatTimeout: registryCfg.Timeout, Logger: logger, Invalidate: applicationRouter.Invalidate, Client: func(address string) (worker.IdleClient, error) {
			return worker.NewHTTPClient(address, cfg.WorkerToken, idleCfg.WakeTimeout)
		}}
		applicationRouter.SetLifecycle(service)
		server.ReadTimeout = max(server.ReadTimeout, idleCfg.WakeTimeout+15*time.Second)
		server.WriteTimeout = max(server.WriteTimeout, idleCfg.WakeTimeout+30*time.Second)
		go func() {
			defer close(idleDone)
			defer owner.Release()
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = owner.Conn().Close(cleanup)
			}()
			done := make(chan struct{})
			go func() { defer close(done); service.Run(ctx, idleCfg.Interval) }()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					<-done
					return
				case <-ticker.C:
					ping, cancel := context.WithTimeout(ctx, 5*time.Second)
					err := owner.Ping(ping)
					cancel()
					if err != nil {
						logger.Error("lost idle router ownership", "error", err)
						stop()
						<-done
						return
					}
				}
			}
		}()
	} else {
		close(idleDone)
	}

	go func() {
		logger.Info("control plane listening", "address", cfg.Address())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("control plane stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	failoverDone := make(chan struct{})
	if failoverCfg.Enabled {
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(failoverCfg.Region))
		if err != nil {
			logger.Error("configure EC2 fencing", "error", err)
			os.Exit(1)
		}
		engine := failover.Engine{Pool: pool, Fencer: failover.EC2Fencer{Client: ec2.NewFromConfig(awsCfg)}, Client: placement.Client, Grace: failoverCfg.Grace, HeartbeatTimeout: registryCfg.Timeout, OperationTimeout: buildTimeout + startTimeout, MaxAttempts: failoverCfg.MaxAttempts, Logger: logger, Invalidate: applicationRouter.Invalidate}
		go func() { defer close(failoverDone); engine.Run(ctx, failoverCfg.Interval) }()
	} else {
		close(failoverDone)
	}
	registryStore := workers.Postgres{Pool: pool}
	registryServer := &http.Server{Addr: registryCfg.Address, Handler: workers.Handler(registryStore, cfg.WorkerToken, workers.Applications{Pool: pool, MaxRestarts: recoveryCfg.MaxRestarts, Cooldown: recoveryCfg.Cooldown}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
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
	<-failoverDone
	<-idleDone
	<-expirationDone

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
