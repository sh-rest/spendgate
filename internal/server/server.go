// Package server holds the HTTP handlers. Week-1 scope: health checks only.
// The proxy hot path, enforcement, and dashboard land in later weeks.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/sh-rest/spendgate/internal/store"
)

// New returns the HTTP handler for the gateway.
func New(st *store.Store) http.Handler {
	mux := http.NewServeMux()

	// Liveness: process is up.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Readiness: dependencies (Postgres) reachable.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := st.Ping(ctx); err != nil {
			http.Error(w, "postgres unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	return mux
}
