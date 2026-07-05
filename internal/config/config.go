// Package config loads runtime configuration from environment variables.
// No config framework by design (DESIGN.md): env vars with sane defaults.
package config

import "os"

type Config struct {
	Port        string
	DatabaseURL string
	RedisURL    string

	OpenAIBaseURL    string
	AnthropicBaseURL string
}

// Load reads configuration from the environment, applying defaults.
func Load() Config {
	return Config{
		Port:             env("PORT", "8080"),
		DatabaseURL:      env("DATABASE_URL", "postgres://spendgate:spendgate@localhost:5432/spendgate?sslmode=disable"),
		RedisURL:         env("REDIS_URL", "redis://localhost:6379"),
		OpenAIBaseURL:    env("OPENAI_BASE_URL", "https://api.openai.com"),
		AnthropicBaseURL: env("ANTHROPIC_BASE_URL", "https://api.anthropic.com"),
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
