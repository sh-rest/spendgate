// Package tenant handles API key generation, hashing, and tenant creation.
package tenant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GenerateKey returns a new plaintext API key of the form sg_<hex>.
func GenerateKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sg_" + hex.EncodeToString(b), nil
}

// HashKey returns the hex-encoded SHA-256 of a plaintext key. Only the hash is
// stored; the plaintext is shown to the user once at creation time.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Create generates a key, inserts the tenant row with the key's hash, and
// returns the plaintext key (to be printed once).
func Create(ctx context.Context, pool *pgxpool.Pool, name string) (string, error) {
	key, err := GenerateKey()
	if err != nil {
		return "", err
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO tenants (name, api_key_hash) VALUES ($1, $2)`,
		name, HashKey(key),
	)
	if err != nil {
		return "", fmt.Errorf("insert tenant: %w", err)
	}
	return key, nil
}
