package dashboard

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool connects to TEST_DATABASE_URL, skipping if unset (matches the
// convention in internal/prices/seed_test.go: CI runs unit tests without a
// real Postgres).
func testPool(t *testing.T) *pgxpool.Pool {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run dashboard aggregation tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("db unreachable: %v", err)
	}
	return pool
}

// seedTenant inserts a tenant and returns its id.
func seedTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) int64 {
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO tenants (name, api_key_hash) VALUES ($1, $2) RETURNING id`,
		name, "hash-"+name,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed tenant %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM requests WHERE tenant_id = $1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, id)
	})
	return id
}

func seedRequest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID int64, feature, provider, model string, cost float64, estimated bool, age time.Duration) {
	_, err := pool.Exec(ctx,
		`INSERT INTO requests (tenant_id, feature, provider, model, cost_usd, estimated, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		tenantID, feature, provider, model, cost, estimated, time.Now().Add(-age),
	)
	if err != nil {
		t.Fatalf("seed request: %v", err)
	}
}

func TestSummarize(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	acme := seedTenant(t, ctx, pool, "acme-dashboard-test")
	globex := seedTenant(t, ctx, pool, "globex-dashboard-test")

	seedRequest(t, ctx, pool, acme, "chatbot", "openai", "gpt-4o-mini", 1.00, false, time.Minute)
	seedRequest(t, ctx, pool, acme, "chatbot", "openai", "gpt-4o-mini", 0.50, true, time.Minute)
	seedRequest(t, ctx, pool, acme, "summarizer", "anthropic", "claude-haiku-4-5", 2.00, false, time.Minute)
	seedRequest(t, ctx, pool, globex, "chatbot", "openai", "gpt-4o-mini", 3.00, false, time.Minute)
	// Outside the window: must not be counted.
	seedRequest(t, ctx, pool, globex, "chatbot", "openai", "gpt-4o-mini", 999.00, false, 48*time.Hour)

	s, err := Summarize(ctx, pool, 24*time.Hour)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	var acmeSpend, globexSpend float64
	var acmeShare float64
	found := map[int64]bool{}
	for _, ts := range s.Tenants {
		switch ts.TenantID {
		case acme:
			acmeSpend = ts.SpendUSD
			acmeShare = ts.EstimatedShare
			found[acme] = true
			if ts.Requests != 3 {
				t.Errorf("acme requests = %d, want 3", ts.Requests)
			}
		case globex:
			globexSpend = ts.SpendUSD
			found[globex] = true
			if ts.Requests != 1 {
				t.Errorf("globex requests = %d, want 1 (old row excluded)", ts.Requests)
			}
		}
	}
	if !found[acme] || !found[globex] {
		t.Fatalf("expected both seeded tenants in summary, got %+v", s.Tenants)
	}
	if acmeSpend != 3.50 {
		t.Errorf("acme spend = %v, want 3.50", acmeSpend)
	}
	if globexSpend != 3.00 {
		t.Errorf("globex spend = %v, want 3.00 (old row excluded)", globexSpend)
	}
	wantShare := 0.50 / 3.50
	if diff := acmeShare - wantShare; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("acme estimated share = %v, want %v", acmeShare, wantShare)
	}

	// Per-feature breakdown includes tenant + feature grouping.
	var sawChatbot, sawSummarizer bool
	for _, f := range s.Features {
		if f.TenantID == acme && f.Feature == "chatbot" {
			sawChatbot = true
			if f.SpendUSD != 1.50 || f.Requests != 2 {
				t.Errorf("acme/chatbot = %+v, want spend 1.50 requests 2", f)
			}
		}
		if f.TenantID == acme && f.Feature == "summarizer" {
			sawSummarizer = true
		}
	}
	if !sawChatbot || !sawSummarizer {
		t.Fatalf("expected acme chatbot+summarizer features, got %+v", s.Features)
	}

	// Per-model spend aggregates across tenants.
	var openaiSpend float64
	for _, m := range s.Models {
		if m.Provider == "openai" && m.Model == "gpt-4o-mini" {
			openaiSpend = m.SpendUSD
		}
	}
	if openaiSpend != 4.50 {
		t.Errorf("openai/gpt-4o-mini spend = %v, want 4.50", openaiSpend)
	}
}
