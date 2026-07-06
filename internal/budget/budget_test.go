package budget

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestClient(t *testing.T) (*Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewFromClient(rdb), mr
}

// TestReserveAtomicUnderConcurrency is the core guarantee: N goroutines racing the
// script against a shared counter must never let total reservations exceed the
// cap. limit=1000, est=100 → exactly 10 of 50 requests may pass, counter lands on
// exactly 1000.
func TestReserveAtomicUnderConcurrency(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()
	key := MonthKey(1, time.Now())

	const limit, est, goroutines = 1000, 100, 50
	var allowed int64
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _, err := c.Reserve(ctx, key, limit, est)
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			if ok {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()

	if allowed != limit/est {
		t.Errorf("allowed = %d, want %d", allowed, limit/est)
	}
	_, spend, err := c.Reserve(ctx, key, limit, 0) // est 0 always allowed; reads spend
	if err != nil {
		t.Fatalf("read spend: %v", err)
	}
	if spend != limit {
		t.Errorf("final spend = %d, want %d (never over cap)", spend, limit)
	}
}

func TestReserveDeniedReportsCurrentSpend(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()
	key := MonthKey(2, time.Now())

	if ok, spend, _ := c.Reserve(ctx, key, 1000, 900); !ok || spend != 900 {
		t.Fatalf("first reserve: ok=%v spend=%d, want true/900", ok, spend)
	}
	ok, spend, err := c.Reserve(ctx, key, 1000, 200) // 900+200 > 1000 → denied
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if ok {
		t.Errorf("reserve allowed over cap")
	}
	if spend != 900 {
		t.Errorf("denied spend = %d, want 900 (unchanged)", spend)
	}
}

// TestReconcileMath: reserve an estimate, then adjust by (actual-estimate). Both a
// refund (actual<estimate) and a top-up (actual>estimate) must land exactly.
func TestReconcileMath(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()
	key := MonthKey(3, time.Now())

	_, _, _ = c.Reserve(ctx, key, 1_000_000, 500) // reserved 500
	if err := c.Reconcile(ctx, key, 300-500); err != nil {
		t.Fatalf("reconcile refund: %v", err)
	}
	if _, spend, _ := c.Reserve(ctx, key, 1_000_000, 0); spend != 300 {
		t.Errorf("after refund spend = %d, want 300", spend)
	}
	if err := c.Reconcile(ctx, key, 700-500); err != nil { // second req: est 500, actual 700
		t.Fatalf("reconcile topup: %v", err)
	}
	// second reservation itself:
	_, _, _ = c.Reserve(ctx, key, 1_000_000, 500)
	if _, spend, _ := c.Reserve(ctx, key, 1_000_000, 0); spend != 300+200+500 {
		t.Errorf("after topup spend = %d, want 1000", spend)
	}
}

func TestReserveSetsTTL(t *testing.T) {
	c, mr := newTestClient(t)
	ctx := context.Background()
	key := MonthKey(4, time.Now())

	if _, _, err := c.Reserve(ctx, key, 1000, 100); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	ttl := mr.TTL(key)
	if ttl <= 0 || ttl > ttlSeconds*time.Second {
		t.Errorf("ttl = %v, want (0, %ds]", ttl, ttlSeconds)
	}
}

// TestMonthRollover: crossing a UTC month boundary uses a fresh key, so a new
// month starts at zero spend regardless of last month's counter.
func TestMonthRollover(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()

	jan := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	feb := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	janKey, febKey := MonthKey(9, jan), MonthKey(9, feb)
	if janKey == febKey {
		t.Fatalf("month keys collided: %s", janKey)
	}

	_, _, _ = c.Reserve(ctx, janKey, 1000, 1000) // fill January
	ok, spend, err := c.Reserve(ctx, febKey, 1000, 500)
	if err != nil {
		t.Fatalf("feb reserve: %v", err)
	}
	if !ok || spend != 500 {
		t.Errorf("february fresh reserve = ok:%v spend:%d, want true/500", ok, spend)
	}
}
