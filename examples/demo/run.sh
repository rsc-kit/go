#!/usr/bin/env bash
# Start the Go demo: a Go server in front, a JS renderer behind it.
#
#   ./run.sh [port]
#
# Open the printed URL. Every page is rendered by the JS process; every value
# on it came from the Go one.
set -euo pipefail

PORT="${1:-8080}"
RENDERER_PORT=$((PORT + 1))
SECRET="${RSC_HOST_SECRET:-demo-secret}"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
adapter="$(cd "$here/../.." && pwd)"
core="$(cd "$adapter/../../packages/core" && pwd)"
bundle="${RSC_BUNDLE_DIR:-$core/.tmp/vite-go-test}"

if [ ! -f "$bundle/dist/rsc/index.js" ]; then
  echo "No build at $bundle — building the fixture app…"
  (cd "$core" && NODE_ENV=production \
    RSC_PROJECT_ROOT="$core" \
    RSC_SOURCE_DIR="$core/tests/fixtures/rsc-app" \
    RSC_OUT_DIR="$bundle" \
    RSC_ASSETS_DIR="$bundle/public" \
    RSC_VITE_CONFIG="$core/tests/fixtures/vite.rsc.config.mjs" \
    bun "$core/src/build-rsc-vite.ts")
fi

cleanup() { kill ${GO_PID:-} ${JS_PID:-} 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# Go first: the renderer needs somewhere to send host calls.
(cd "$adapter" && go run ./examples/hostserver \
  -secret "$SECRET" \
  -addr "127.0.0.1:$PORT" \
  -renderer "http://127.0.0.1:$RENDERER_PORT") &
GO_PID=$!

(cd "$core" && bun "$here/renderer.ts" \
  --bundle "$bundle" \
  --host-call "http://127.0.0.1:$PORT/__rsc/host-call" \
  --secret "$SECRET" \
  --port "$RENDERER_PORT") &
JS_PID=$!

sleep 2
echo
echo "  Go        http://127.0.0.1:$PORT      ← open this"
echo "  renderer  http://127.0.0.1:$RENDERER_PORT  (behind it)"
echo
wait
