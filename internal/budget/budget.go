// Package budget enforces per-tenant monthly spend caps using Redis. Each tenant
// has a counter keyed budget:{tenant_id}:{YYYY-MM} (UTC month) holding accumulated
// cost in micro-USD (integer — floats are never stored in Redis, to avoid drift).
// Reservation is an atomic check-and-INCRBY via a single Lua script, so two
// replicas can never both slip a request past the cap.
package budget

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ttlSeconds is ~62 days: long enough to outlive any calendar month, so a month's
// counter self-expires and Redis stays clean without a sweeper.
const ttlSeconds = 62 * 24 * 60 * 60

// Client wraps a Redis connection for budget counters.
type Client struct {
	rdb *redis.Client
}

// New parses a redis:// URL and returns a Client. It does not dial; connection
// failures surface on first use, where the per-tenant fail-open/closed policy
// decides what to do.
func New(redisURL string) (*Client, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	return &Client{rdb: redis.NewClient(opt)}, nil
}

// NewFromClient wraps an existing redis client (tests use a miniredis-backed one).
func NewFromClient(rdb *redis.Client) *Client { return &Client{rdb: rdb} }

// Close releases the connection pool.
func (c *Client) Close() error { return c.rdb.Close() }

// Ping checks Redis reachability (used by /readyz).
func (c *Client) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

// MonthKey builds the counter key for a tenant in the UTC month of t.
func MonthKey(tenantID int64, t time.Time) string {
	return fmt.Sprintf("budget:%d:%s", tenantID, t.UTC().Format("2006-01"))
}

// reserveScript atomically reserves est against the counter iff cur+est <= limit,
// returning {allowed, spend}. On the first write it sets a ~62-day TTL so the key
// self-expires after the month. Running the whole check-and-increment inside one
// script is what makes the cap exact across replicas: no read-modify-write race.
var reserveScript = redis.NewScript(`
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
local limit = tonumber(ARGV[1])
local est = tonumber(ARGV[2])
if cur + est > limit then
  return {0, cur}
end
local new = redis.call('INCRBY', KEYS[1], est)
if redis.call('TTL', KEYS[1]) < 0 then
  redis.call('EXPIRE', KEYS[1], ARGV[3])
end
return {1, new}
`)

// Reserve atomically checks and reserves estMicro micro-USD against key's counter.
// Returns whether the request is allowed and the counter value in micro-USD: the
// post-reservation spend on allow, the current spend on deny.
func (c *Client) Reserve(ctx context.Context, key string, limitMicro, estMicro int64) (allowed bool, spendMicro int64, err error) {
	res, err := reserveScript.Run(ctx, c.rdb, []string{key}, limitMicro, estMicro, ttlSeconds).Result()
	if err != nil {
		return false, 0, err
	}
	vals, ok := res.([]interface{})
	if !ok || len(vals) != 2 {
		return false, 0, fmt.Errorf("budget: unexpected script result %v", res)
	}
	allowedI, _ := vals[0].(int64)
	spendMicro, _ = vals[1].(int64)
	return allowedI == 1, spendMicro, nil
}

// Reconcile adjusts the counter by deltaMicro (actual - estimate) once the real
// metered cost is known. deltaMicro may be negative (over-reserved) and a zero
// delta is a no-op.
// ponytail: plain INCRBY, no Lua. If the key expired between Reserve and Reconcile
// (only possible across the ~62-day TTL boundary) this recreates it without a TTL;
// accepted — the next month's first Reserve re-sets the TTL.
func (c *Client) Reconcile(ctx context.Context, key string, deltaMicro int64) error {
	if deltaMicro == 0 {
		return nil
	}
	return c.rdb.IncrBy(ctx, key, deltaMicro).Err()
}
