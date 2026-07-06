# spendgate

Self-hosted LLM cost-attribution gateway, written in Go.

## Status

Built: config, schema/migrations, `create-tenant` CLI, health endpoints, the
proxy hot path (OpenAI + Anthropic, streaming + non-streaming), async batch
metering to Postgres, and Redis per-tenant monthly budget enforcement. Budget
checks run before forwarding via a single atomic Lua reserve-and-check (exact
across replicas), reconciled to the real cost once the response completes; over
budget returns 429, and on a Redis outage each tenant's `fail_open` flag picks
forward-unmetered-budget vs 503. `/readyz` reports Postgres and Redis health.
Not built yet: the live dashboard and benchmark numbers.

## Quickstart

```
make up
make migrate-up
go run ./cmd/spendgate create-tenant acme
make run
```

Then check:

```
curl localhost:8080/healthz
curl localhost:8080/readyz
```

## Env vars

| Var | Default |
|---|---|
| `PORT` | `8080` |
| `DATABASE_URL` | `postgres://spendgate:spendgate@localhost:5432/spendgate?sslmode=disable` |
| `REDIS_URL` | `redis://localhost:6379` |
| `OPENAI_BASE_URL` | none |
| `ANTHROPIC_BASE_URL` | none |
