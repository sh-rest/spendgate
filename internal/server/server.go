// Package server holds the HTTP handlers: health checks plus the authenticated
// proxy hot path. Enforcement and dashboard land in later weeks.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/sh-rest/spendgate/internal/auth"
	"github.com/sh-rest/spendgate/internal/proxy"
	"github.com/sh-rest/spendgate/internal/store"
)

// New returns the HTTP handler for the gateway. The proxy is mounted for the
// OpenAI and Anthropic route prefixes behind the auth middleware.
func New(st *store.Store, a *auth.Authenticator, p *proxy.Proxy) http.Handler {
	mux := http.NewServeMux()

	proxyH := a.Middleware(p)
	mux.Handle("/openai/", proxyH)
	mux.Handle("/anthropic/", proxyH)

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
