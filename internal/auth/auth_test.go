package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sh-rest/spendgate/internal/tenant"
)

// TestCacheTTL: a resolved tenant is served from cache within the TTL and
// re-fetched from the source of truth after it expires.
func TestCacheTTL(t *testing.T) {
	var calls int32
	lookup := func(context.Context, string) (tenant.Tenant, bool, error) {
		atomic.AddInt32(&calls, 1)
		return tenant.Tenant{ID: 1}, true, nil
	}
	a := New(lookup, 30*time.Second)

	now := time.Unix(0, 0)
	a.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if _, ok, _ := a.resolve(context.Background(), "sg_key"); !ok {
			t.Fatal("expected tenant")
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("within TTL: lookups = %d, want 1", got)
	}

	now = now.Add(31 * time.Second) // past the TTL
	if _, ok, _ := a.resolve(context.Background(), "sg_key"); !ok {
		t.Fatal("expected tenant after expiry")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("after TTL: lookups = %d, want 2", got)
	}
}

// TestMiddlewareUnknownKey: an unknown key is never cached and returns 401.
func TestMiddlewareUnknownKey(t *testing.T) {
	var calls int32
	a := New(func(context.Context, string) (tenant.Tenant, bool, error) {
		atomic.AddInt32(&calls, 1)
		return tenant.Tenant{}, false, nil
	}, time.Minute)

	h := a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not run for unknown key")
	}))

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/openai/v1/x", nil)
		req.Header.Set("Authorization", "Bearer sg_nope")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401", rec.Code)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("unknown keys should not be cached: lookups = %d, want 2", got)
	}
}

func TestMiddlewareRejectsNonSGKey(t *testing.T) {
	a := New(func(context.Context, string) (tenant.Tenant, bool, error) {
		return tenant.Tenant{ID: 1}, true, nil
	}, time.Minute)
	h := a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openai/v1/x", nil)
	req.Header.Set("Authorization", "Bearer sk-not-ours")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 for non-sg key", rec.Code)
	}
}

// TestMiddlewareStripsClientAuth: the client Authorization header is removed and
// the tenant is available on the context for downstream handlers.
func TestMiddlewareStripsClientAuth(t *testing.T) {
	a := New(func(context.Context, string) (tenant.Tenant, bool, error) {
		return tenant.Tenant{ID: 42}, true, nil
	}, time.Minute)

	var sawAuth string
	var sawTenant tenant.Tenant
	h := a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawTenant, _ = TenantFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openai/v1/x", nil)
	req.Header.Set("Authorization", "Bearer sg_valid")
	h.ServeHTTP(rec, req)

	if sawAuth != "" {
		t.Errorf("client Authorization not stripped: %q", sawAuth)
	}
	if sawTenant.ID != 42 {
		t.Errorf("tenant not on context: %+v", sawTenant)
	}
}
