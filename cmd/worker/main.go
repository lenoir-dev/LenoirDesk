package main

import (
	"context"
	"github.com/redis/go-redis/v9"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lenoir-dev/LenoirDesk/internal/config"
	"github.com/lenoir-dev/LenoirDesk/internal/crypto"
	"github.com/lenoir-dev/LenoirDesk/internal/delivery"
	"github.com/lenoir-dev/LenoirDesk/internal/execution"
	"github.com/lenoir-dev/LenoirDesk/internal/providerconn"
	"github.com/lenoir-dev/LenoirDesk/internal/providers"
	"github.com/lenoir-dev/LenoirDesk/internal/reconciliation"
	"github.com/lenoir-dev/LenoirDesk/internal/recoverywitness"
	"github.com/lenoir-dev/LenoirDesk/internal/retention"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
	"github.com/lenoir-dev/LenoirDesk/internal/telemetry"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	shutdownTrace, err := telemetry.Setup()
	if err != nil {
		log.Error("telemetry setup failed", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := shutdownTrace(context.Background()); err != nil {
			log.Warn("trace shutdown failed", "error", err)
		}
	}()
	c, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	telemetry.SetWorkerOutboundEnabled(false)
	if addr := os.Getenv("RELAYDESK_METRICS_ADDR"); addr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", telemetry.MetricsHandler())
		server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 3 * time.Second}
		go func() {
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error("metrics listener failed", "error", err)
				stop()
			}
		}()
		defer func() { _ = server.Shutdown(context.Background()) }()
	}
	if os.Getenv("RELAYDESK_OUTBOUND_EXECUTION_ENABLED") != "true" {
		log.Warn("worker idle: outbound execution requires explicit deployment clearance")
		<-ctx.Done()
		return
	}
	if c.ProviderMode != "simulation" && c.ProviderMode != "test" {
		log.Error("outbound execution requires a supported provider mode")
		os.Exit(1)
	}
	s, err := store.Open(ctx, c.DatabaseURL)
	if err != nil {
		log.Error("database unavailable", "error", err)
		os.Exit(1)
	}
	defer s.Close()
	var witness *recoverywitness.Witness
	if c.ProviderMode == "simulation" || c.ProviderMode == "test" {
		witnessPath := os.Getenv("RELAYDESK_RESTORE_WITNESS_PATH")
		if witnessPath == "" {
			log.Error("external restore witness path required for outbound worker")
			os.Exit(1)
		}
		witness = &recoverywitness.Witness{Pool: s.Pool, Path: witnessPath}
		if err := witness.Check(ctx); err != nil {
			log.Error("external restore witness closed; recovery review required")
			os.Exit(1)
		}
	}
	worker, nc, err := delivery.New(ctx, s, c.NATSURL, log)
	if err != nil {
		log.Error("NATS initialization failed", "error", err)
		os.Exit(1)
	}
	defer nc.Close()
	policy, err := retention.PolicyFromEnv()
	if err != nil {
		log.Error("retention policy invalid", "error", err)
		os.Exit(1)
	}
	worker.Retention = &policy
	if c.ProviderMode == "simulation" || c.ProviderMode == "test" {
		box, boxErr := crypto.NewBox(c.EncryptionKey)
		if boxErr != nil {
			log.Error("encryption unavailable", "error", boxErr)
			os.Exit(1)
		}
		redisOptions, redisErr := redis.ParseURL(c.RedisURL)
		if redisErr != nil {
			log.Error("Redis configuration invalid", "error", redisErr)
			os.Exit(1)
		}
		redisClient := redis.NewClient(redisOptions)
		defer func() { _ = redisClient.Close() }()
		endpoints := providers.Endpoints{Stripe: c.StripeAPIURL, HubSpot: c.HubSpotAPIURL, Slack: c.SlackAPIURL, Simulation: true}
		executor := &execution.Executor{DB: s, Box: box, Log: log, RequireLimiter: true,
			Limiter: &execution.RedisWriteLimiter{Client: redisClient, Limit: 5, Window: time.Second}, Endpoints: endpoints,
			Witness: witness}
		if c.ProviderMode == "test" {
			executor.Endpoints = providers.Endpoints{Stripe: "https://api.stripe.com", HubSpot: "https://api.hubapi.com", Slack: "https://slack.com"}
			executor.Quota = &execution.ProviderQuota{Client: redisClient}
			executor.Breaker = &execution.ProviderBreaker{Client: redisClient}
			executor.Refresher = &providerconn.Refresher{DB: s, Box: box, Client: providerconn.Client{Config: providerconn.Config{
				HubSpotID: c.HubSpotClientID, HubSpotSecret: c.HubSpotClientSecret,
				SlackID: c.SlackClientID, SlackSecret: c.SlackClientSecret,
			}}}
			executor.HubSpotAppID, executor.SlackAppID = c.HubSpotClientID, c.SlackClientID
		}
		worker.Business = executor
		worker.Reconcile = &reconciliation.Reconciler{DB: s, Box: box, Endpoints: executor.Endpoints,
			Quota: executor.Quota, Log: log}
	}
	telemetry.SetWorkerOutboundEnabled(true)
	defer telemetry.SetWorkerOutboundEnabled(false)
	worker.Run(ctx)
}
