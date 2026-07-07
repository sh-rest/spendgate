package prices

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sh-rest/spendgate/internal/store"
)

// TestSeedUpsert verifies re-seeding updates existing rows rather than
// duplicating or failing. Requires a Postgres reachable via TEST_DATABASE_URL;
// skipped otherwise (CI runs unit tests without a DB).
func TestSeedUpsert(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run seeding upsert test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if err := (&store.Store{Pool: pool}).Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	p := []Price{{Provider: "test", Model: "m1", InputUSDPerMTok: 1, OutputUSDPerMTok: 2}}
	if err := Seed(ctx, pool, p); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	// Re-seed with a changed price; must upsert, not error or duplicate.
	p[0].InputUSDPerMTok = 5
	if err := Seed(ctx, pool, p); err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	var count int
	var input float64
	if err := pool.QueryRow(ctx,
		`SELECT count(*), max(input_usd_per_mtok) FROM model_prices WHERE provider='test' AND model='m1'`,
	).Scan(&count, &input); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row after upsert, got %d", count)
	}
	if input != 5 {
		t.Fatalf("expected updated price 5, got %v", input)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM model_prices WHERE provider='test'`)
}
