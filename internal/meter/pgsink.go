package meter

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGSink writes metering events into the requests table via a single COPY.
type PGSink struct{ Pool *pgxpool.Pool }

func (s PGSink) Write(ctx context.Context, events []Event) error {
	_, err := s.Pool.CopyFrom(ctx,
		pgx.Identifier{"requests"},
		[]string{"tenant_id", "feature", "provider", "model", "input_tokens",
			"output_tokens", "cost_usd", "estimated", "status", "latency_ms", "created_at"},
		pgx.CopyFromSlice(len(events), func(i int) ([]any, error) {
			e := events[i]
			return []any{e.TenantID, e.Feature, e.Provider, e.Model, e.InputTokens,
				e.OutputTokens, e.CostUSD, e.Estimated, e.Status, e.LatencyMS, e.CreatedAt}, nil
		}),
	)
	return err
}
