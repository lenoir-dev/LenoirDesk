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

	"github.com/lenoir-dev/LenoirDesk/internal/api"
	"github.com/lenoir-dev/LenoirDesk/internal/config"
	"github.com/lenoir-dev/LenoirDesk/internal/crypto"
	"github.com/lenoir-dev/LenoirDesk/internal/identity"
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
	if err := c.ValidateAPI(); err != nil {
		log.Error("invalid API configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	s, err := store.Open(ctx, c.DatabaseURL)
	if err != nil {
		log.Error("database unavailable", "error", err)
		os.Exit(1)
	}
	defer s.Close()
	authStore, err := store.Open(ctx, c.AuthDatabaseURL)
	if err != nil {
		log.Error("identity database unavailable", "error", err)
		os.Exit(1)
	}
	defer authStore.Close()
	appStore, err := store.Open(ctx, c.AppDatabaseURL)
	if err != nil {
		log.Error("application database unavailable", "error", err)
		os.Exit(1)
	}
	defer appStore.Close()
	box, err := crypto.NewBox(c.EncryptionKey)
	if err != nil {
		log.Error("encryption unavailable", "error", err)
		os.Exit(1)
	}
	var auth *identity.Service
	for attempts := 0; attempts < 20; attempts++ {
		setupCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
		auth, err = identity.New(setupCtx, authStore, appStore, box, c)
		cancel()
		if err == nil || ctx.Err() != nil {
			break
		}
		log.Warn("OIDC discovery unavailable; retrying")
		time.Sleep(time.Second)
	}
	if err != nil {
		log.Error("OIDC provider unavailable", "error", err)
		os.Exit(1)
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			for batch := 0; batch < 4; batch++ {
				cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				removed, cleanupErr := auth.CleanupExpiredFlows(cleanupCtx)
				cancel()
				if cleanupErr != nil {
					if ctx.Err() == nil {
						log.Warn("OAuth flow cleanup unavailable")
					}
					break
				}
				if removed < 1500 { // three tables, at most 500 rows each
					break
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	srv := api.New(c, s, box, log, auth)
	done := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	select {
	case err = <-done:
		if err != nil {
			log.Error("server stopped", "error", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = srv.Shutdown(shutdownCtx)
		cancel()
		if err != nil {
			log.Error("shutdown failed", "error", err)
		}
	}
}
