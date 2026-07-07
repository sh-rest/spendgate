# spendgate benchmark harness

Measures **gateway overhead**: the latency spendgate adds on top of the
upstream provider, isolated from real network variance.

```
overhead = latency(client -> spendgate -> fake provider)
         - latency(client -> fake provider directly)
```

Both legs run on the same box, same load shape, back to back.

## What's here

- `fakeprovider/` — a tiny Go server mimicking OpenAI's
  `POST /v1/chat/completions`. Configurable latency (`-latency`), returns a
  valid `usage` block, and a real SSE stream (content deltas + a final
  usage-only frame) when the request has `"stream":true`. Because it returns
  real usage, spendgate's full metering path (usage extraction, price lookup,
  async Postgres batch write) runs during the benchmark.
- `loadgen/` — a stdlib closed-loop load generator (no vegeta/k6 installed).
  `-c` workers loop request-after-request for `-d`, each fully draining the
  response body (so streaming is measured end to end). Reports p50/p95/p99 and
  req/s.
- `run.sh` — orchestrates the whole thing (see below). Invoked by `make bench`.

## Running

```
make bench
```

This will: `make up` (Postgres + Redis) → migrate → create a bench tenant →
start the fake provider and the gateway (`OPENAI_BASE_URL` pointed at the fake
provider, dashboard disabled) → warm up → run four load phases (non-streaming
and streaming, each direct and via-gateway, 500 connections × 30s) → write
`bench/results-<date>.md`.

Tunable via env: `CONC` (default 500), `DURATION` (30s), `LATENCY` (10ms fake
provider latency), `FAKE_PORT`, `GW_PORT`.

### Why no 429s pollute results

A tenant created with `create-tenant` has `monthly_budget_usd = NULL`, which the
proxy treats as "no cap" (`checkBudget` returns early, forwards unreserved). So
the bench tenant is unbudgeted by construction — no budget rejections, but the
metering write path still runs on every request.

## Methodology & caveats

These are **dev-laptop numbers**, honest about their limits:

- **Loopback only.** Client, gateway, and fake provider all talk over
  `localhost`. There is no real network hop, TLS, or DNS. A real deployment
  sits between client and provider over the internet; this harness deliberately
  removes that variance to expose *just* the gateway's own processing cost.
- **Same-box contention.** The load generator, the gateway, the fake provider,
  Postgres, and Redis all share the same CPU and memory. Under 500 connections
  they compete for cores, which inflates tail latencies for *both* the direct
  and via-gateway legs. The subtraction cancels much of this, but not all —
  treat the overhead as an estimate with same-box noise, not a lab-clean figure.
- **Fake provider ≠ real provider.** The upstream is a local Go handler with a
  fixed sleep, not a real model API. Real providers stream tokens over hundreds
  of milliseconds to seconds; against that, gateway overhead is proportionally
  far smaller than it looks here.
- **Closed-loop, not open-loop.** The generator is closed-loop (each worker
  waits for its response before firing the next), so it measures latency under a
  fixed concurrency, not behavior at a fixed arrival rate. It does not model
  coordinated omission.
- **Percentiles are nearest-rank** over all successful requests, single run, no
  confidence intervals. Re-run for stability; expect a few percent variance
  between runs.

The direct-vs-gateway subtraction is the honest part: whatever same-box noise
exists hits both legs, so the *difference* is a fair estimate of spendgate's
added cost on this machine.
