#!/usr/bin/env bash
# demo/run.sh — the 30-second wow moment: live dashboard ticking up as traffic
# flows across two tenants, ending with one tenant blowing its budget and
# getting 429'd.
#
# Runs on its own ports (15440/16390/18080/18081) so it doesn't collide with
# whatever's already using 5432/6379/8080 on this machine. Everything —
# containers, fakeprovider, gateway — is torn down on exit.
set -euo pipefail
cd "$(dirname "$0")/.."

PG_PORT=15440
REDIS_PORT=16390
GW_PORT=18080
DASH_PORT=18081
FAKE_PORT=19090
PG_NAME=spendgate-demo-pg
REDIS_NAME=spendgate-demo-redis
TMP="$(mktemp -d)"

FAKE_PID=""
GW_PID=""
TRAFFIC_PID=""
cleanup() {
  echo ">> cleaning up"
  [[ -n "$TRAFFIC_PID" ]] && kill "$TRAFFIC_PID" 2>/dev/null || true
  [[ -n "$GW_PID" ]] && kill "$GW_PID" 2>/dev/null || true
  [[ -n "$FAKE_PID" ]] && kill "$FAKE_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  docker rm -f "$PG_NAME" "$REDIS_NAME" >/dev/null 2>&1 || true
  rm -rf "$TMP"
}
trap cleanup EXIT

echo ">> starting Postgres + Redis (ports $PG_PORT/$REDIS_PORT)"
docker run -d --name "$PG_NAME" -p "$PG_PORT:5432" \
  -e POSTGRES_USER=spendgate -e POSTGRES_PASSWORD=spendgate -e POSTGRES_DB=spendgate \
  postgres:16 >/dev/null
docker run -d --name "$REDIS_NAME" -p "$REDIS_PORT:6379" redis:7 >/dev/null

DATABASE_URL="postgres://spendgate:spendgate@localhost:$PG_PORT/spendgate?sslmode=disable"
REDIS_URL="redis://localhost:$REDIS_PORT"

for i in $(seq 1 30); do
  docker exec "$PG_NAME" pg_isready -U spendgate >/dev/null 2>&1 && break
  sleep 1
done

echo ">> building"
go build -o "$TMP/fakeprovider" ./bench/fakeprovider
go build -o "$TMP/spendgate" ./cmd/spendgate

echo ">> migrating"
DATABASE_URL="$DATABASE_URL" "$TMP/spendgate" migrate >/dev/null

echo ">> creating tenants: acme (\$0.05 budget), globex (no budget)"
ACME_KEY="$(DATABASE_URL="$DATABASE_URL" "$TMP/spendgate" create-tenant acme | grep -o 'sg_[0-9a-f]*' | head -1)"
GLOBEX_KEY="$(DATABASE_URL="$DATABASE_URL" "$TMP/spendgate" create-tenant globex | grep -o 'sg_[0-9a-f]*' | head -1)"
[[ -n "$ACME_KEY" && -n "$GLOBEX_KEY" ]] || { echo "failed to capture tenant keys"; exit 1; }
docker exec "$PG_NAME" psql -U spendgate -d spendgate -c \
  "UPDATE tenants SET monthly_budget_usd = 0.05 WHERE name = 'acme';" >/dev/null

echo ">> starting fakeprovider on :$FAKE_PORT (~200ms latency)"
# Bigger per-request usage (200/5000 tokens) than bench's default 12/8, purely so
# acme's $0.05 cap trips in ~15-20 requests instead of ~7500 — makes for a snappy
# recording without changing what a "request" costs in the pricing model.
"$TMP/fakeprovider" -addr ":$FAKE_PORT" -latency 200ms -prompt-tokens 200 -completion-tokens 5000 &
FAKE_PID=$!

echo ">> starting gateway on :$GW_PORT (dashboard on :$DASH_PORT)"
DATABASE_URL="$DATABASE_URL" REDIS_URL="$REDIS_URL" \
  OPENAI_BASE_URL="http://localhost:$FAKE_PORT" \
  PORT="$GW_PORT" DASHBOARD_ADDR="127.0.0.1:$DASH_PORT" \
  "$TMP/spendgate" serve >"$TMP/gw.log" 2>&1 &
GW_PID=$!

for i in $(seq 1 30); do
  curl -fsS "http://localhost:$GW_PORT/readyz" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "http://localhost:$GW_PORT/readyz" >/dev/null || { echo "gateway not ready"; cat "$TMP/gw.log"; exit 1; }

echo ">> dashboard live at http://127.0.0.1:$DASH_PORT"
echo ">> driving traffic (acme + globex, alternating features) until acme hits its cap"

FEATURES=(chat summarizer search)
send() {
  local key="$1" feature="$2"
  curl -sS -o /tmp/spendgate-demo-resp.json -w "%{http_code}" \
    -X POST "http://localhost:$GW_PORT/openai/v1/chat/completions" \
    -H "Authorization: Bearer $key" \
    -H "X-Spendgate-Feature: $feature" \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
}

(
  i=0
  cap_hit_at=-1
  while true; do
    feature="${FEATURES[$((i % 3))]}"
    if [[ $((i % 2)) -eq 0 ]]; then
      code="$(send "$ACME_KEY" "$feature")"
      if [[ "$code" == "429" && "$cap_hit_at" == "-1" ]]; then
        cap_hit_at=$i
        echo
        echo ">> acme just hit its \$0.05 cap — gateway returned 429:"
        cat /tmp/spendgate-demo-resp.json
        echo
      fi
      echo -n "acme/$feature:$code "
    else
      code="$(send "$GLOBEX_KEY" "$feature")"
      echo -n "globex/$feature:$code "
    fi
    i=$((i + 1))
    sleep 0.15
    # Keep running ~15s past the first 429 (each iteration costs ~0.35-0.4s
    # once curl round-trip + fakeprovider latency are counted) so the
    # recording has time to show the capped tenant staying capped.
    if [[ "$cap_hit_at" != "-1" && $((i - cap_hit_at)) -ge 40 ]]; then
      break
    fi
  done
  echo
  echo ">> demo traffic finished"
) &
TRAFFIC_PID=$!

wait "$TRAFFIC_PID"
