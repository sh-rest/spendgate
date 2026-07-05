# spendgate

Self-hosted LLM cost-attribution gateway, written in Go.

## Status

Week-1 skeleton: config, schema/migrations, `create-tenant` CLI, health
endpoints, and an async batch metering writer. Not built yet: the proxy hot
path, streaming, Redis-based enforcement, and the dashboard.

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
