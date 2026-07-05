package tenant

import (
	"strings"
	"testing"
)

func TestGenerateKeyFormatAndUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		key, err := GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		if !strings.HasPrefix(key, "sg_") {
			t.Fatalf("key %q missing sg_ prefix", key)
		}
		if len(key) != len("sg_")+48 { // 24 bytes hex-encoded
			t.Fatalf("key %q unexpected length %d", key, len(key))
		}
		if seen[key] {
			t.Fatalf("duplicate key generated: %q", key)
		}
		seen[key] = true
	}
}

func TestHashKeyStableAndHex(t *testing.T) {
	h1 := HashKey("sg_abc")
	h2 := HashKey("sg_abc")
	if h1 != h2 {
		t.Fatal("HashKey not deterministic")
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(h1))
	}
	if HashKey("sg_abc") == HashKey("sg_abd") {
		t.Fatal("different keys hashed to same value")
	}
	// Plaintext must not appear in the hash.
	if strings.Contains(h1, "sg_abc") {
		t.Fatal("hash leaks plaintext")
	}
}
