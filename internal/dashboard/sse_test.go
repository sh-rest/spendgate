package dashboard

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLiveSSE asserts /api/live frames events as "event: summary" /
// "data: {...}" pairs and pushes more than one event over a short injected
// tick interval (proving periodic delivery without waiting 2s per DESIGN.md).
func TestLiveSSE(t *testing.T) {
	pool := testPool(t)
	seedTenant(t, context.Background(), pool, "sse-test-tenant")

	h := Handler(pool, 20*time.Millisecond)
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/live", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/live: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	var events, dataLines int
	for sc.Scan() && dataLines < 3 {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			if strings.TrimPrefix(line, "event: ") != "summary" {
				t.Fatalf("unexpected event name: %q", line)
			}
			events++
		case strings.HasPrefix(line, "data: "):
			dataLines++
			if !strings.Contains(line, "total_spend_usd") {
				t.Fatalf("data line missing expected field: %q", line)
			}
		}
	}
	if events < 2 {
		t.Fatalf("expected at least 2 periodic events with a 20ms tick, got %d", events)
	}
	if dataLines < events {
		t.Fatalf("expected a data line per event, got %d events %d data lines", events, dataLines)
	}
}
