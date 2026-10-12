package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lenoir-dev/LenoirDesk/internal/config"
	"github.com/lenoir-dev/LenoirDesk/internal/crypto"
	"github.com/lenoir-dev/LenoirDesk/internal/identity"
	"github.com/lenoir-dev/LenoirDesk/internal/ingress"
	"github.com/lenoir-dev/LenoirDesk/internal/store"
)

func New(c config.Config, s *store.Store, b *crypto.Box, log *slog.Logger, auth *identity.Service) *http.Server {
	router := chi.NewRouter()
	router.Use(ingress.Correlation)
	router.Get("/health/live", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	router.Get("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.Ready(ctx); err != nil {
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h := ingress.Handler{Store: s, Box: b, Log: log}
	router.Post("/webhooks/stripe/{connection_ref}", h.Stripe)
	if auth != nil {
		app := &application{cfg: c, identity: auth, log: log}
		router.Group(func(r chi.Router) {
			r.Use(app.securityHeaders)
			app.routes(r)
		})
	}
	return &http.Server{Addr: c.HTTPAddr, Handler: router, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: c.ReadTimeout, WriteTimeout: c.WriteTimeout, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
}
