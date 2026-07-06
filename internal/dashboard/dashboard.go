package dashboard

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed index.html
var indexHTML []byte

// DefaultTickInterval is how often /api/live re-polls Postgres and pushes a
// refresh. DESIGN.md requires spend visible within 2s of a request landing;
// polling every 2s satisfies that without needing a pub/sub fan-out.
//
// ponytail: simple ticker poll, one query per tick, no subscriber fan-out.
// DESIGN.md explicitly defers Redis pub/sub fan-out; upgrade path is to have
// the meter writer publish a "requests changed" event on a Redis channel and
// have connected SSE handlers subscribe instead of polling.
const DefaultTickInterval = 2 * time.Second

// Handler builds the dashboard's HTTP handler. tick overrides the live-poll
// interval when non-zero (tests inject a short interval).
func Handler(pool *pgxpool.Pool, tick time.Duration) http.Handler {
	if tick <= 0 {
		tick = DefaultTickInterval
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/summary", summaryHandler(pool))
	mux.HandleFunc("GET /api/live", liveHandler(pool, tick))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	return mux
}

func parseWindow(r *http.Request) (time.Duration, error) {
	v := r.URL.Query().Get("window")
	if v == "" {
		return 24 * time.Hour, nil
	}
	return time.ParseDuration(v)
}

func summaryHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		window, err := parseWindow(r)
		if err != nil {
			http.Error(w, "invalid window: "+err.Error(), http.StatusBadRequest)
			return
		}
		s, err := Summarize(r.Context(), pool, window)
		if err != nil {
			http.Error(w, "summary query failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s)
	}
}

// liveHandler streams a "summary" SSE event every tick while the client is
// connected, ending cleanly when the request context is cancelled.
func liveHandler(pool *pgxpool.Pool, tick time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		window, err := parseWindow(r)
		if err != nil {
			http.Error(w, "invalid window: "+err.Error(), http.StatusBadRequest)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		send := func() bool {
			s, err := Summarize(r.Context(), pool, window)
			if err != nil {
				return true // skip a bad tick, stay connected
			}
			body, err := json.Marshal(s)
			if err != nil {
				return true
			}
			if _, err := fmt.Fprintf(w, "event: summary\ndata: %s\n\n", body); err != nil {
				return false
			}
			flusher.Flush()
			return true
		}

		if !send() {
			return
		}

		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if !send() {
					return
				}
			}
		}
	}
}
