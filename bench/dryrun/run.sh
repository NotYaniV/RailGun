#!/usr/bin/env bash
# Validate the harness end to end inside one Linux container.
#
# NOT a benchmark. Load generator, proxy and origin share a kernel and talk over
# loopback, so every number here is noise. A pass means the machinery works:
# nginx loads our config, wrk2 runs our Lua, the parsers accept real output,
# /proc sampling finds the workers, and the report renders.
set -euo pipefail

DURATION="${DURATION:-10s}"
REPEATS="${REPEATS:-2}"
OUT="${OUT:-/tmp/dryrun-results}"

echo "=== nginx config check (this is where 'use epoll' would fail on a non-Linux host) ==="
cp /src/bench/nginx/nginx.conf /etc/nginx/nginx.conf
nginx -t

echo "=== starting origin on 0.0.0.0:9001 ==="
origin -addr 0.0.0.0:9001 &
echo "=== starting nginx on :8080 ==="
nginx

# Wait for both to answer rather than sleeping a guessed interval.
for target in 127.0.0.1:9001 127.0.0.1:8080; do
    for _ in $(seq 50); do
        curl -fsS -o /dev/null "http://$target/bytes/16" && break
        sleep 0.1
    done
    curl -fsS -o /dev/null "http://$target/bytes/16" \
        || { echo "FAIL: $target never came up" >&2; exit 1; }
    echo "  $target responding"
done

echo "=== nginx workers visible to /proc sampling ==="
pgrep -x nginx | tr '\n' ' '; echo

rm -rf "$OUT"
echo "=== running harness (duration=$DURATION repeats=$REPEATS) ==="
bench run \
    --proxy-url  http://127.0.0.1:8080 \
    --null-url   http://127.0.0.1:9001 \
    --remote-cmd 'sh -c' \
    --proxy-host '' \
    --proc-name  nginx \
    --wrk-script-dir /src/bench/wrk \
    --duration "$DURATION" \
    --repeats  "$REPEATS" \
    --out      "$OUT"

echo
echo "=== report ==="
bench report --in "$OUT"

echo
echo "=== dry run complete. Numbers above are meaningless by construction; ==="
echo "=== what passed is the machinery: config, wrk2, Lua, /proc, parsers, report. ==="
