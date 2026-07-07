# Demo recording

`docs/demo.gif` is the 30-second wow moment: two tenants sending traffic
through spendgate, the live dashboard ticking up in real time, and acme's
$0.05 budget tripping mid-recording — its spend bar flatlines while globex's
keeps climbing, and the gateway starts returning 429s.

## Re-recording

```
./demo/record.sh
```

This drives `demo/run.sh` in the background (own Postgres/Redis containers on
15440/16390, gateway on 18080, dashboard on 18081 — won't collide with a
locally running spendgate), waits for the dashboard to come up, then
screenshots it every ~500ms for 30s using gstack's `browse` tool
(`~/.claude/skills/gstack/browse/dist/browse` — a persistent headless
Chromium daemon, already set up on this machine, ~100ms per screenshot after
the first call). Frames go through `ffmpeg` to produce `docs/demo.gif`
(currently ~110KB, well under the 10MB budget).

If `browse` isn't available in your environment, the fallback is Playwright:
`npm install playwright && npx playwright install chromium` in a scratch
directory, then swap the screenshot loop in `demo/record.sh` for a small node
script that keeps one `chromium.launch()` browser open and calls
`page.screenshot()` in a loop — launching a fresh browser per shot (`npx
playwright screenshot ...`) is too slow (~5s/shot) to hit a 500ms cadence.

## Running just the traffic (no recording)

```
./demo/run.sh
```

Boots everything, creates `acme` ($0.05 budget) and `globex` (no budget),
drives alternating traffic tagged with `chat`/`summarizer`/`search` features,
and prints the 429 JSON to the terminal the moment acme's cap trips. Leave the
dashboard open at `http://127.0.0.1:18081` in a browser to watch it live.
Everything (containers, gateway, fakeprovider) is torn down on exit —
Ctrl-C is safe.

Fakeprovider is started with `-prompt-tokens 200 -completion-tokens 5000`
(bench's default is 12/8) purely to make the $0.05 cap trip in ~15-20
requests instead of ~7500 — it doesn't change what a real request costs
against `prices.yaml`, just how much fake usage this demo's fake requests
report.
