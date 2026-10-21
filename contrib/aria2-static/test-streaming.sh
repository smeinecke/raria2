#!/bin/sh
# Verify that aria2c dispatches URLs written to a still-open stdin pipe
# BEFORE EOF (incremental input, aria2/aria2#1161). Exits non-zero on
# unpatched binaries, which only dispatch after stdin closes.
#
# Usage: test-streaming.sh [path-to-aria2c]
set -eu

ARIA2C=${1:-aria2c}
PORT=${PORT:-18191}
HOLD=${HOLD:-8}        # seconds stdin stays open after writing the URL
CHECK_AT=${CHECK_AT:-4} # when the file must already exist (< HOLD)

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"; [ -n "${SRV_PID:-}" ] && kill "$SRV_PID" 2>/dev/null || true; [ -n "${A2_PID:-}" ] && kill "$A2_PID" 2>/dev/null || true' EXIT

echo "payload" > "$TMP/f1"
(cd "$TMP" && python3 -m http.server "$PORT" >/dev/null 2>&1) &
SRV_PID=$!
sleep 1

mkdir "$TMP/dl"
# Feed one URL, then keep stdin open by sleeping while holding the pipe.
( printf 'http://127.0.0.1:%s/f1\n' "$PORT"; exec sleep "$HOLD" ) | \
    "$ARIA2C" --input-file - --deferred-input=true \
    --dir="$TMP/dl" --console-log-level=warn --summary-interval=0 \
    --file-allocation=none --max-tries=1 &
A2_PID=$!

sleep "$CHECK_AT"
if [ -f "$TMP/dl/f1" ]; then
    echo "PASS: download dispatched while stdin still open"
    wait "$A2_PID" 2>/dev/null || true
    A2_PID=
    exit 0
fi

echo "FAIL: no download after ${CHECK_AT}s with open stdin (unpatched aria2?)"
exit 1
