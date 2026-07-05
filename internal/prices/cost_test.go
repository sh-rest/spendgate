package prices

import (
	"math"
	"testing"
)

func TestCost(t *testing.T) {
	table := BuildTable([]Price{
		{Provider: "openai", Model: "gpt-4o-mini", InputUSDPerMTok: 0.15, OutputUSDPerMTok: 0.60},
		{Provider: "anthropic", Model: "claude-haiku-4-5", InputUSDPerMTok: 1.0, OutputUSDPerMTok: 5.0},
	})

	tests := []struct {
		name             string
		provider, model  string
		in, out          int
		want             float64
		wantFound        bool
	}{
		{"openai known", "openai", "gpt-4o-mini", 1000, 500, 1000.0/1e6*0.15 + 500.0/1e6*0.60, true},
		{"anthropic known", "anthropic", "claude-haiku-4-5", 9, 14, 9.0/1e6*1.0 + 14.0/1e6*5.0, true},
		{"zero tokens", "openai", "gpt-4o-mini", 0, 0, 0, true},
		{"unknown model", "openai", "gpt-5", 100, 100, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, found := table.Cost(tc.provider, tc.model, tc.in, tc.out)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("cost = %v, want %v", got, tc.want)
			}
		})
	}
}
