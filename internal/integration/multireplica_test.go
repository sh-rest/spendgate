// Package integration holds tests that exercise spendgate end to end against
// real Postgres + Redis (no fakes below the HTTP boundary). Gated by
// TEST_DATABASE_URL, same convention as internal/prices/seed_test.go — skipped
// under `go test ./...` without a live DB, run via `make test-integration`
// (needs `make up`).
package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/sh-rest/spendgate/internal/auth"
	"github.com/sh-rest/spendgate/internal/budget"
	"github.com/sh-rest/spendgate/internal/meter"
	"github.com/sh-rest/spendgate/internal/prices"
	"github.com/sh-rest/spendgate/internal/proxy"
	"github.com/sh-rest/spendgate/internal/server"
	"github.com/sh-rest/spendgate/internal/store"
	"github.com/sh-rest/spendgate/internal/tenant"
)

// replica is one independently-wired spendgate instance: its own Postgres pool,
// Redis client, and metering writer — nothing shared with its sibling except
// the underlying database/cache the config URLs point at. That's what makes
// "2 replicas" honest rather than 2 handles onto one struct.
type replica struct {
	srv      *httptest.Server
	writer   *meter.Writer
	stopMete func()
}

func newReplica(t *testing.T, dbURL, redisURL, upstreamURL string, table prices.Table, lookup auth.Lookup) *replica {
	t.Helper()

	st, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(st.Close)

	bud, err := budget.New(redisURL)
	if err != nil {
		t.Fatalf("open redis: %v", err)
	}
	t.Cleanup(func() { _ = bud.Close() })

	writer := meter.New(meter.PGSink{Pool: st.Pool}, meter.DefaultBatchSize, meter.DefaultInterval)
	ctx, cancel := context.WithCancel(context.Background())
	go writer.Run(ctx)

	px := proxy.New(writer, table, nil, bud,
		proxy.Provider{Name: "openai", Prefix: "/openai", BaseURL: upstreamURL, APIKey: "provider-secret"})
	authr := auth.New(lookup, 30*time.Second)

	srv := httptest.NewServer(server.New(st, bud, authr, px))
	t.Cleanup(srv.Close)

	return &replica{srv: srv, writer: writer, stopMete: func() { cancel(); writer.Wait() }}
}

// TestBudgetEnforcedAcrossReplicas fires a burst of concurrent requests split
// across two independently-wired spendgate replicas sharing one real Postgres
// and one real Redis. It asserts the DESIGN.md success criterion verbatim:
// "Budget cap enforced exactly under concurrent load across 2 replicas."
func TestBudgetEnforcedAcrossReplicas(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("set TEST_DATABASE_URL (and run `make up`) to run the multi-replica integration test")
	}
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379"
	}
	ctx := context.Background()

	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Fresh tenant per run: a new serial ID means a fresh (empty) Redis budget
	// key by construction, so there's nothing to flush to start "clean".
	name := fmt.Sprintf("multireplica-test-%d", time.Now().UnixNano())
	key, err := tenant.Create(ctx, st.Pool, name)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	var tenantID int64
	if err := st.Pool.QueryRow(ctx, `SELECT id FROM tenants WHERE name = $1`, name).Scan(&tenantID); err != nil {
		t.Fatalf("lookup tenant id: %v", err)
	}

	const (
		capMicro  = 500 // $0.0005
		costMicro = 100 // exact per-allowed-request cost, chosen below
		k         = 40  // K * cost = 4000 micro >> 500 cap
	)
	if _, err := st.Pool.Exec(ctx,
		`UPDATE tenants SET monthly_budget_usd = $1, fail_open = FALSE WHERE id = $2`,
		float64(capMicro)/1e6, tenantID,
	); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool.Exec(context.Background(), `DELETE FROM requests WHERE tenant_id = $1`, tenantID)
		_, _ = st.Pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
	})

	// Price table: output priced at 0 so only input tokens matter, and the
	// request body is padded to exactly 400 bytes so the proxy's chars/4
	// reservation estimate (len(body)/4 = 100) equals the fake provider's
	// reported prompt_tokens (100) exactly — estimate == actual, so
	// reconciliation is a zero-delta no-op and the Redis counter never drifts
	// from the sum of real costs.
	table := prices.BuildTable([]prices.Price{
		{Provider: "openai", Model: "load-test", InputUSDPerMTok: 1.0, OutputUSDPerMTok: 0},
	})
	const bodyLen = 400 // -> len/4 == 100 prompt tokens, matching the fixture below
	base := `{"model":"load-test","pad":""}`
	body := `{"model":"load-test","pad":"` + strings.Repeat("a", bodyLen-len(base)) + `"}`
	if len(body) != bodyLen {
		t.Fatalf("test body len = %d, want %d", len(body), bodyLen)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"load-test","usage":{"prompt_tokens":100,"completion_tokens":0}}`))
	}))
	defer upstream.Close()

	lookup := tenant.LookupByHash(st.Pool)
	r1 := newReplica(t, dbURL, redisURL, upstream.URL, table, lookup)
	r2 := newReplica(t, dbURL, redisURL, upstream.URL, table, lookup)

	client := &http.Client{Timeout: 10 * time.Second}
	fire := func(replicaURL string) int {
		req, _ := http.NewRequest(http.MethodPost, replicaURL+"/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("request: %v", err)
			return 0
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	var allowed, denied, other int64
	var wg sync.WaitGroup
	for i := 0; i < k; i++ {
		wg.Add(1)
		replicaURL := r1.srv.URL
		if i%2 == 1 {
			replicaURL = r2.srv.URL
		}
		go func(u string) {
			defer wg.Done()
			switch fire(u) {
			case http.StatusOK:
				atomic.AddInt64(&allowed, 1)
			case http.StatusTooManyRequests:
				atomic.AddInt64(&denied, 1)
			default:
				atomic.AddInt64(&other, 1)
			}
		}(replicaURL)
	}
	wg.Wait()

	if other != 0 {
		t.Fatalf("got %d unexpected non-200/429 responses", other)
	}
	if allowed+denied != k {
		t.Fatalf("allowed(%d) + denied(%d) = %d, want %d", allowed, denied, allowed+denied, k)
	}
	wantAllowed := int64(capMicro / costMicro)
	if allowed != wantAllowed {
		t.Errorf("allowed = %d, want exactly %d (cap %d / cost %d)", allowed, wantAllowed, capMicro, costMicro)
	}

	// Flush both replicas' metering buffers before checking Postgres.
	r1.stopMete()
	r2.stopMete()

	rdb := redis.NewClient(mustParseRedisURL(t, redisURL))
	defer rdb.Close()
	redisSpend, err := rdb.Get(ctx, budget.MonthKey(tenantID, time.Now())).Int64()
	if err != nil {
		t.Fatalf("read redis counter: %v", err)
	}
	if redisSpend > capMicro {
		t.Errorf("redis counter = %d micro-USD, exceeds cap %d", redisSpend, capMicro)
	}
	if redisSpend != wantAllowed*costMicro {
		t.Errorf("redis counter = %d, want %d (exact, no drift)", redisSpend, wantAllowed*costMicro)
	}

	var pgRows int64
	var pgSpendUSD float64
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*), coalesce(sum(cost_usd), 0) FROM requests WHERE tenant_id = $1`, tenantID,
	).Scan(&pgRows, &pgSpendUSD); err != nil {
		t.Fatalf("query requests: %v", err)
	}
	if pgRows != allowed {
		t.Errorf("postgres rows = %d, want %d (only allowed requests are metered — 429s never enqueue)", pgRows, allowed)
	}
	pgSpendMicro := int64(pgSpendUSD*1e6 + 0.5)
	if pgSpendMicro != redisSpend {
		t.Errorf("postgres metered spend = %d micro, want %d (matches reconciled redis counter)", pgSpendMicro, redisSpend)
	}
}

func mustParseRedisURL(t *testing.T, raw string) *redis.Options {
	t.Helper()
	opt, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("parse TEST_REDIS_URL: %v", err)
	}
	return opt
}
