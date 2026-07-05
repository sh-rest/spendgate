package prices

// Key identifies a price row.
type Key struct {
	Provider string
	Model    string
}

// Table is an in-memory price lookup, built once at startup (DESIGN.md: prices
// are cached in memory; Postgres/YAML is the source of truth).
type Table map[Key]Price

// BuildTable indexes a price list by (provider, model).
func BuildTable(list []Price) Table {
	t := make(Table, len(list))
	for _, p := range list {
		t[Key{p.Provider, p.Model}] = p
	}
	return t
}

// Cost returns the USD cost for a token count and whether a price was found.
// Prices are per million tokens. When the model is unknown, cost is 0 and
// found is false — the caller still records the tokens.
func (t Table) Cost(provider, model string, inTokens, outTokens int) (float64, bool) {
	p, ok := t[Key{provider, model}]
	if !ok {
		return 0, false
	}
	cost := float64(inTokens)/1e6*p.InputUSDPerMTok +
		float64(outTokens)/1e6*p.OutputUSDPerMTok
	return cost, true
}
