# spendgate

Self-hosted LLM cost-attribution gateway, written in Go. Point your OpenAI or
Anthropic SDK's `base_url` at it and every request gets metered, attributed to
a tenant/feature, and budget-enforced — exactly, across replicas — before
it's forwarded. One static binary, `docker compose up` to a working gateway
in under a minute.

**Why not Helicone or LiteLLM?** Helicone was acquired in March 2026 and is in
maintenance mode, with 50-80ms of added latency from its architecture.
LiteLLM is a Python proxy: GIL contention degrades throughput 1.7-4x under
load, and its usage logging sits in Postgres in the request path. spendgate's
hot path touches only Redis (one atomic round trip); everything else is
async. Both are more full-featured (evals, prompt management, more
providers) — spendgate deliberately isn't, in exchange for a simpler,
faster, self-hosted core.

## 60-second quickstart

```
make up               # Postgres + Redis via docker compose
make migrate-up
go run ./cmd/spendgate create-tenant acme   # prints the key once: sg_...
make run               # gateway on :8080
```

Point an SDK at the gateway instead of the provider directly:

```python
# OpenAI
client = OpenAI(base_url="http://localhost:8080/openai/v1", api_key="sg_...")

# Anthropic
client = Anthropic(base_url="http://localhost:8080/anthropic/v1", api_key="sg_...")
```

Open the dashboard at `http://127.0.0.1:8081` to watch spend land per
tenant/feature in real time.

## Features

- **Per-tenant / per-feature attribution** — tag requests with
  `X-Spendgate-Feature`; cost rolls up by tenant and feature.
- **Exact multi-replica budget enforcement** — monthly budget checked via a
  single atomic Redis Lua script (check-and-reserve, reconciled after) before forwarding, so
  concurrent requests across replicas can't blow past the cap. Reconciled to
  real cost once the response completes.
- **Streaming usage extraction** — OpenAI and Anthropic both emit token usage
  in the final SSE frame; spendgate injects `stream_options` to request it if
  the client didn't ask, then strips the synthetic frame back out before
  passing the stream to the client (ADR-003) — so metering doesn't leak into
  what the caller sees.
- **Fail-open / fail-closed per tenant** — if Redis is down, each tenant's
  flag decides between forwarding unmetered (fail-open) or a 503
  (fail-closed).
- **Live dashboard** — SSE, per-tenant/feature spend breakdown, no auth,
  binds to localhost by default.
- **Single static binary** — no runtime dependencies besides Postgres and
  Redis; `docker-compose.yml` included.

## Benchmarks

Verified numbers, dev machine (Apple M5, loopback, fake provider —
methodology and caveats in `bench/README.md`):

| scenario | p95 overhead | throughput | errors |
|---|---|---|---|
| Isolated hot-path (50 conns, non-streaming) | < 1ms | ~4.3k req/s | 0 |
| Isolated hot-path (50 conns, streaming) | 1.7ms | ~4.3k req/s | 0 |
| Saturation (500 closed-loop conns, non-streaming) | 7.5ms | ~37k req/s | 0 |

Design target was p95 < 5ms at 500 req/s — met with headroom at the
throughput that matters; the 500-connection saturation pass pushes far past
that to confirm zero errors under load. These are dev-machine loopback
numbers, not production/cloud measurements — see `bench/README.md` for the
methodology, and `bench/README.md`'s History section for the connection-pool
bug this same benchmark caught and the fix that landed for it.

## Design decisions

- **Hot path touches only Redis.** One Lua round trip does the budget
  reserve-and-check; Postgres writes are async and batched (N rows or T ms,
  whichever first). This is the direct answer to LiteLLM's DB-in-request-path
  problem. Trade-off: a crash can lose up to one batch of metering data —
  accepted, this is metering, not a billing ledger.
- **Reserve-then-reconcile budgets**, tracked as micro-USD integers to avoid
  float drift. Reserve an estimate before forwarding, reconcile to the real
  metered cost after.
- **Streaming usage inject+strip (ADR-003).** Provider-reported usage is the
  source of truth for token counts; spendgate asks for it via
  `stream_options` if needed and removes the extra frame before the client
  sees it.
- **Fail-open/fail-closed is a per-tenant config flag**, not a global
  setting — different tenants can have different risk tolerance for a Redis
  outage.
- **Upstream connection pooling**, learned the hard way: the benchmark
  surfaced that Go's default `http.Transport` (`MaxIdleConnsPerHost = 2`)
  collapses under concurrent upstream forwarding. Fixed in `proxy.New` by
  raising `MaxIdleConns`/`MaxIdleConnsPerHost` to 1024. Full writeup in
  `bench/README.md`.

## Tests

```
go test ./...              # unit tests + -race, no external services
make up                    # start Postgres + Redis
TEST_DATABASE_URL=... make test-integration   # gated on env var
```

The integration test runs two independently-wired spendgate instances
sharing one Postgres and Redis, fires a concurrent burst that would blow
past a tenant's budget many times over, and asserts the cap holds exactly:
allowed spend never exceeds the cap, every request resolves to exactly one
200 or one 429, and the reconciled Redis counter matches real metered cost
in Postgres.

## Non-goals

Rate limiting (budget caps only), evals, prompt management, and more than
two providers (OpenAI + Anthropic). These are deliberate scope cuts, not
gaps — kept the core small and correct instead.

## Releases

GoReleaser builds on `v*` tags: binaries (darwin/linux) plus a Docker image
published to `ghcr.io`.

## Env vars

| Var | Default |
|---|---|
| `PORT` | `8080` |
| `DATABASE_URL` | `postgres://spendgate:spendgate@localhost:5432/spendgate?sslmode=disable` |
| `REDIS_URL` | `redis://localhost:6379` |
| `OPENAI_BASE_URL` | none |
| `ANTHROPIC_BASE_URL` | none |
</content>
