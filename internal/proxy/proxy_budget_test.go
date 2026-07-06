package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/sh-rest/spendgate/internal/budget"
	"github.com/sh-rest/spendgate/internal/tenant"
)

func usd(v float64) *float64 { return &v }

func upBudget(t *testing.T) (*budget.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return budget.NewFromClient(rdb), mr
}

// downBudget returns a client whose Redis is unreachable (server started then
// closed), so every call errors — exercising the fail-open/closed paths.
func downBudget(t *testing.T) *budget.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	mr.Close()
	t.Cleanup(func() { _ = rdb.Close() })
	return budget.NewFromClient(rdb)
}

func jsonOKProvider(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// usage 9/10 for gpt-4o-mini-2024-07-18 (priced in the harness table).
	_, _ = w.Write([]byte(`{"model":"gpt-4o-mini-2024-07-18","usage":{"prompt_tokens":9,"completion_tokens":10}}`))
}

// TestBudgetExceeded429: an estimate over the cap is denied before forwarding —
// no upstream call, no metering, and the documented JSON body.
func TestBudgetExceeded429(t *testing.T) {
	bud, _ := upBudget(t)
	h := newBudgetHarness(t, "openai", jsonOKProvider, bud,
		tenant.Tenant{ID: 7, Name: "acme", MonthlyBudgetUSD: usd(0.0005)}) // 500 micro cap, est ~600
	defer h.stop()

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, newProxyRequest("openai", `{"model":"gpt-4o-mini-2024-07-18"}`))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429", rec.Code)
	}
	var body struct {
		Error struct {
			Type      string  `json:"type"`
			Tenant    string  `json:"tenant"`
			BudgetUSD float64 `json:"budget_usd"`
			SpendUSD  float64 `json:"spend_usd"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body.String())
	}
	if body.Error.Type != "budget_exceeded" || body.Error.Tenant != "acme" || body.Error.BudgetUSD != 0.0005 {
		t.Errorf("body = %+v, want type=budget_exceeded tenant=acme budget_usd=0.0005", body.Error)
	}
	if h.lastReq != nil {
		t.Errorf("request forwarded despite over-budget")
	}
	time.Sleep(20 * time.Millisecond)
	if evs := h.sink.snapshot(); len(evs) != 0 {
		t.Errorf("metered %d events, want 0 (blocked)", len(evs))
	}
}

// TestBudgetAllowedAndReconcile: an in-budget request forwards, and the counter
// is reconciled from the estimate down to the actual metered cost (9/10 tokens =
// 7 micro-USD).
func TestBudgetAllowedAndReconcile(t *testing.T) {
	bud, mr := upBudget(t)
	h := newBudgetHarness(t, "openai", jsonOKProvider, bud,
		tenant.Tenant{ID: 7, Name: "acme", MonthlyBudgetUSD: usd(1.0)})
	defer h.stop()

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, newProxyRequest("openai", `{"model":"gpt-4o-mini-2024-07-18"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}

	// actual cost = 9/1e6*0.15 + 10/1e6*0.60 = 7.35e-6 USD -> 7 micro-USD.
	key := budget.MonthKey(7, time.Now())
	got, err := mr.Get(key)
	if err != nil {
		t.Fatalf("get counter: %v", err)
	}
	if got != "7" {
		t.Errorf("reconciled counter = %s, want 7 (estimate released to actual)", got)
	}
	if ev := h.oneEvent(t); ev.OutputTokens != 10 {
		t.Errorf("metered out=%d, want 10", ev.OutputTokens)
	}
}

// TestFailOpenForwardsAndMeters: Redis down + fail_open true → request still
// forwards and is metered to Postgres (budget unenforced, warning logged).
func TestFailOpenForwardsAndMeters(t *testing.T) {
	h := newBudgetHarness(t, "openai", jsonOKProvider, downBudget(t),
		tenant.Tenant{ID: 7, Name: "acme", FailOpen: true, MonthlyBudgetUSD: usd(1.0)})
	defer h.stop()

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, newProxyRequest("openai", `{"model":"gpt-4o-mini-2024-07-18"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (fail-open forwards)", rec.Code)
	}
	if h.lastReq == nil {
		t.Errorf("request not forwarded under fail-open")
	}
	if ev := h.oneEvent(t); ev.OutputTokens != 10 {
		t.Errorf("fail-open request not metered correctly: out=%d", ev.OutputTokens)
	}
}

// TestFailClosed503: Redis down + fail_open false → 503, no forward, no metering.
func TestFailClosed503(t *testing.T) {
	h := newBudgetHarness(t, "openai", jsonOKProvider, downBudget(t),
		tenant.Tenant{ID: 7, Name: "acme", FailOpen: false, MonthlyBudgetUSD: usd(1.0)})
	defer h.stop()

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, newProxyRequest("openai", `{"model":"gpt-4o-mini-2024-07-18"}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (fail-closed)", rec.Code)
	}
	var body struct {
		Error struct{ Type string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.Type != "budget_unavailable" {
		t.Errorf("error type = %q, want budget_unavailable", body.Error.Type)
	}
	if h.lastReq != nil {
		t.Errorf("request forwarded despite fail-closed")
	}
	time.Sleep(20 * time.Millisecond)
	if evs := h.sink.snapshot(); len(evs) != 0 {
		t.Errorf("metered %d events, want 0", len(evs))
	}
}
