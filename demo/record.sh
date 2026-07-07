#!/usr/bin/env bash
# demo/record.sh — drives demo/run.sh, screenshots the dashboard every ~500ms
# for ~40s via gstack's `browse` (persistent headless Chromium daemon, already
# set up on this machine — reuse it instead of standing up a second
# Playwright/Chromium install), then stitches the frames into docs/demo.gif
# with ffmpeg.
set -euo pipefail
cd "$(dirname "$0")/.."

B="$HOME/.claude/skills/gstack/browse/dist/browse"
[ -x "$B" ] || { echo "gstack browse not found at $B — see demo/README.md for the Playwright fallback"; exit 1; }

FRAMES_DIR="$(mktemp -d /tmp/spendgate-demo-frames.XXXXXX)" # browse only writes under /tmp or the repo
DASH_URL="http://127.0.0.1:18081"
DEMO_PID=""

cleanup() {
  [[ -n "$DEMO_PID" ]] && kill "$DEMO_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$FRAMES_DIR"
}
trap cleanup EXIT

echo ">> starting demo traffic in the background"
./demo/run.sh > /tmp/spendgate-demo.log 2>&1 &
DEMO_PID=$!

echo ">> waiting for dashboard at $DASH_URL"
for i in $(seq 1 40); do
  curl -fsS "$DASH_URL" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "$DASH_URL" >/dev/null || { echo "dashboard never came up"; cat /tmp/spendgate-demo.log; exit 1; }

# Let traffic ramp for a beat before the first frame so acme has non-zero spend
# in frame 1, then capture through the 429 tail.
sleep 2

echo ">> capturing screenshots to $FRAMES_DIR (40s, 500ms interval)"
"$B" viewport 900x650 >/dev/null
"$B" goto "$DASH_URL" >/dev/null
for i in $(seq 0 59); do
  printf -v padded "%04d" "$i"
  "$B" screenshot --viewport "$FRAMES_DIR/frame-$padded.png" >/dev/null
  sleep 0.4
done

echo ">> waiting for demo traffic to finish"
wait "$DEMO_PID" || true
DEMO_PID=""

echo ">> encoding docs/demo.gif"
mkdir -p docs
ffmpeg -y -framerate 2 -i "$FRAMES_DIR/frame-%04d.png" \
  -vf "scale=640:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=128[p];[b][p]paletteuse" \
  -loop 0 docs/demo.gif

ls -lh docs/demo.gif
echo ">> done"
