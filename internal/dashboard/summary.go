// Package dashboard serves the embedded read-only spend dashboard: JSON
// summary/SSE endpoints plus a single self-contained HTML page. It runs on
// its own listener (see New in server.go) so it isn't bound by the proxy's
// localhost-only default.
package dashboard

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Summary is the JSON shape returned by /api/summary and pushed by /api/live.
type Summary struct {
	Window        string         `json:"window"`
	TotalSpendUSD float64        `json:"total_spend_usd"`
	TotalRequests int64          `json:"total_requests"`
	Tenants       []TenantSpend  `json:"tenants"`
	Features      []FeatureSpend `json:"features"`
	Models        []ModelSpend   `json:"models"`
	GeneratedAt   time.Time      `json:"generated_at"`
}

type TenantSpend struct {
	TenantID       int64   `json:"tenant_id"`
	Name           string  `json:"name"`
	SpendUSD       float64 `json:"spend_usd"`
	Requests       int64   `json:"requests"`
	EstimatedShare float64 `json:"estimated_share"` // fraction of tenant spend that is estimated (0-1)
}

type FeatureSpend struct {
	TenantID int64   `json:"tenant_id"`
	Tenant   string  `json:"tenant"`
	Feature  string  `json:"feature"`
	SpendUSD float64 `json:"spend_usd"`
	Requests int64   `json:"requests"`
}

type ModelSpend struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	SpendUSD float64 `json:"spend_usd"`
	Requests int64   `json:"requests"`
}

// Summarize aggregates the requests table over the trailing window. All
// aggregation happens in SQL (three targeted queries hitting the
// (tenant_id, created_at) index via the created_at >= $1 predicate) rather
// than pulling rows into Go.
func Summarize(ctx context.Context, pool *pgxpool.Pool, window time.Duration) (*Summary, error) {
	since := time.Now().Add(-window)
	s := &Summary{Window: window.String(), GeneratedAt: time.Now()}

	if err := pool.QueryRow(ctx,
		`SELECT coalesce(sum(cost_usd), 0), count(*) FROM requests WHERE created_at >= $1`,
		since,
	).Scan(&s.TotalSpendUSD, &s.TotalRequests); err != nil {
		return nil, err
	}

	tenantRows, err := pool.Query(ctx,
		`SELECT t.id, t.name,
		        coalesce(sum(r.cost_usd), 0) AS spend,
		        count(r.id) AS requests,
		        coalesce(sum(r.cost_usd) FILTER (WHERE r.estimated), 0) AS estimated_spend
		 FROM tenants t
		 JOIN requests r ON r.tenant_id = t.id AND r.created_at >= $1
		 GROUP BY t.id, t.name
		 ORDER BY spend DESC`,
		since,
	)
	if err != nil {
		return nil, err
	}
	defer tenantRows.Close()
	for tenantRows.Next() {
		var t TenantSpend
		var estimatedSpend float64
		if err := tenantRows.Scan(&t.TenantID, &t.Name, &t.SpendUSD, &t.Requests, &estimatedSpend); err != nil {
			return nil, err
		}
		if t.SpendUSD > 0 {
			t.EstimatedShare = estimatedSpend / t.SpendUSD
		}
		s.Tenants = append(s.Tenants, t)
	}
	if err := tenantRows.Err(); err != nil {
		return nil, err
	}

	featureRows, err := pool.Query(ctx,
		`SELECT t.id, t.name, r.feature,
		        sum(r.cost_usd) AS spend, count(*) AS requests
		 FROM requests r
		 JOIN tenants t ON t.id = r.tenant_id
		 WHERE r.created_at >= $1
		 GROUP BY t.id, t.name, r.feature
		 ORDER BY spend DESC`,
		since,
	)
	if err != nil {
		return nil, err
	}
	defer featureRows.Close()
	for featureRows.Next() {
		var f FeatureSpend
		if err := featureRows.Scan(&f.TenantID, &f.Tenant, &f.Feature, &f.SpendUSD, &f.Requests); err != nil {
			return nil, err
		}
		s.Features = append(s.Features, f)
	}
	if err := featureRows.Err(); err != nil {
		return nil, err
	}

	modelRows, err := pool.Query(ctx,
		`SELECT provider, model, sum(cost_usd) AS spend, count(*) AS requests
		 FROM requests
		 WHERE created_at >= $1
		 GROUP BY provider, model
		 ORDER BY spend DESC`,
		since,
	)
	if err != nil {
		return nil, err
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var m ModelSpend
		if err := modelRows.Scan(&m.Provider, &m.Model, &m.SpendUSD, &m.Requests); err != nil {
			return nil, err
		}
		s.Models = append(s.Models, m)
	}
	if err := modelRows.Err(); err != nil {
		return nil, err
	}

	return s, nil
}
