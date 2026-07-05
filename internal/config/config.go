// Package config loads runtime configuration from environment variables.
// No config framework by design (DESIGN.md): env vars with sane defaults.
package config

import (
	"bufio"
	"os"
	"strings"
)

type Config struct {
	Port        string
	DatabaseURL string
	RedisURL    string

	OpenAIBaseURL    string
	AnthropicBaseURL string

	OpenAIKey    string
	AnthropicKey string
}

// Load reads configuration from the environment, applying defaults.
func Load() Config {
	return Config{
		Port:             env("PORT", "8080"),
		DatabaseURL:      env("DATABASE_URL", "postgres://spendgate:spendgate@localhost:5432/spendgate?sslmode=disable"),
		RedisURL:         env("REDIS_URL", "redis://localhost:6379"),
		OpenAIBaseURL:    env("OPENAI_BASE_URL", "https://api.openai.com"),
		AnthropicBaseURL: env("ANTHROPIC_BASE_URL", "https://api.anthropic.com"),
		OpenAIKey:        os.Getenv("OPENAI_API_KEY"),
		AnthropicKey:     os.Getenv("ANTHROPIC_API_KEY"),
	}
}

// LoadDotenv reads KEY=VALUE lines from path into the process environment,
// without overwriting variables already set. Missing file is not an error.
// ponytail: ~20-line parser instead of a dotenv dependency; no quoting/expansion.
func LoadDotenv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			_ = os.Setenv(k, v)
		}
	}
	return sc.Err()
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
