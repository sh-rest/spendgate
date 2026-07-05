package prices

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.yaml")
	writeFile(t, path, `prices:
  - provider: openai
    model: gpt-4o
    input_usd_per_mtok: 2.50
    output_usd_per_mtok: 10.00
  - provider: anthropic
    model: claude-3-5-haiku-20241022
    input_usd_per_mtok: 0.80
    output_usd_per_mtok: 4.00
`)

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 prices, got %d", len(got))
	}
	if got[0] != (Price{Provider: "openai", Model: "gpt-4o", InputUSDPerMTok: 2.50, OutputUSDPerMTok: 10.00}) {
		t.Fatalf("unexpected first price: %+v", got[0])
	}
	if got[1].OutputUSDPerMTok != 4.00 {
		t.Fatalf("bad output price: %v", got[1].OutputUSDPerMTok)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/no/such/prices.yaml"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
