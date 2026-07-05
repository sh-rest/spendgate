// Package prices seeds the model_prices table from a checked-in YAML file.
// Prices are data, not code (DESIGN.md): edit YAML + restart to update.
package prices

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

type Price struct {
	Provider         string  `yaml:"provider"`
	Model            string  `yaml:"model"`
	InputUSDPerMTok  float64 `yaml:"input_usd_per_mtok"`
	OutputUSDPerMTok float64 `yaml:"output_usd_per_mtok"`
}

type file struct {
	Prices []Price `yaml:"prices"`
}

// Load parses a prices YAML file.
func Load(path string) ([]Price, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f file
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return f.Prices, nil
}

// Seed upserts prices into model_prices keyed by (provider, model).
func Seed(ctx context.Context, pool *pgxpool.Pool, prices []Price) error {
	batch := &pgx.Batch{}
	for _, p := range prices {
		batch.Queue(
			`INSERT INTO model_prices (provider, model, input_usd_per_mtok, output_usd_per_mtok)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (provider, model) DO UPDATE SET
			   input_usd_per_mtok = EXCLUDED.input_usd_per_mtok,
			   output_usd_per_mtok = EXCLUDED.output_usd_per_mtok`,
			p.Provider, p.Model, p.InputUSDPerMTok, p.OutputUSDPerMTok,
		)
	}
	br := pool.SendBatch(ctx, batch)
	defer br.Close()
	for range prices {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("seed prices: %w", err)
		}
	}
	return nil
}
