// Package auth resolves a client Bearer key (sg_*) to a tenant. The key is
// SHA-256 hashed and looked up against Postgres (source of truth) behind a
// short in-memory TTL cache. Revocation therefore takes effect within the TTL
// (30s in v1, accepted per DESIGN.md).
package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sh-rest/spendgate/internal/tenant"
)

type ctxKey struct{}

// TenantFrom returns the authenticated tenant stored on the request context.
func TenantFrom(ctx context.Context) (tenant.Tenant, bool) {
	t, ok := ctx.Value(ctxKey{}).(tenant.Tenant)
	return t, ok
}

// Lookup resolves a key hash to a tenant. A missing tenant returns (_, false, nil).
type Lookup func(ctx context.Context, keyHash string) (tenant.Tenant, bool, error)

// Authenticator caches successful tenant lookups for a TTL.
type Authenticator struct {
	lookup Lookup
	ttl    time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]entry
}

type entry struct {
	t       tenant.Tenant
	expires time.Time
}

// New builds an Authenticator. ttl<=0 falls back to 30s.
func New(lookup Lookup, ttl time.Duration) *Authenticator {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Authenticator{
		lookup: lookup,
		ttl:    ttl,
		now:    time.Now,
		cache:  make(map[string]entry),
	}
}

// resolve returns the tenant for a plaintext key, consulting the cache first.
// Only positive results are cached; unknown keys always hit the source of truth.
func (a *Authenticator) resolve(ctx context.Context, key string) (tenant.Tenant, bool, error) {
	hash := tenant.HashKey(key)

	a.mu.Lock()
	if e, ok := a.cache[hash]; ok && a.now().Before(e.expires) {
		a.mu.Unlock()
		return e.t, true, nil
	}
	a.mu.Unlock()

	t, found, err := a.lookup(ctx, hash)
	if err != nil || !found {
		return tenant.Tenant{}, false, err
	}

	a.mu.Lock()
	a.cache[hash] = entry{t: t, expires: a.now().Add(a.ttl)}
	a.mu.Unlock()
	return t, true, nil
}

// Middleware authenticates the request, attaches the tenant to the context, and
// strips the client Authorization header so it never reaches the provider.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := bearerKey(r.Header.Get("Authorization"))
		if !ok {
			http.Error(w, "missing sg_ bearer key", http.StatusUnauthorized)
			return
		}
		t, found, err := a.resolve(r.Context(), key)
		if err != nil {
			http.Error(w, "auth backend unavailable", http.StatusServiceUnavailable)
			return
		}
		if !found {
			http.Error(w, "unknown API key", http.StatusUnauthorized)
			return
		}
		r.Header.Del("Authorization")
		ctx := context.WithValue(r.Context(), ctxKey{}, t)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearerKey extracts an "sg_"-prefixed key from an Authorization header value.
func bearerKey(h string) (string, bool) {
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return "", false
	}
	key := strings.TrimSpace(h[len(p):])
	if !strings.HasPrefix(key, "sg_") {
		return "", false
	}
	return key, true
}
