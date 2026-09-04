#!/usr/bin/env sh
# Render nginx.conf for this host and print it to stdout.
#
#   render.sh [worker_processes]
#
# worker_processes must equal the number of cores the proxy is pinned to, which
# is the same core set Railgun gets (spec §4.3). Defaults to nproc, which is
# correct only when the proxy is not pinned — see bench/RUNBOOK.md §4.
set -eu
workers="${1:-$(nproc)}"

case "$workers" in
    ''|*[!0-9]*) echo "render.sh: worker_processes must be a positive integer, got '$workers'" >&2; exit 2 ;;
    0) echo "render.sh: worker_processes must be > 0" >&2; exit 2 ;;
esac

sed "s/__WORKER_PROCESSES__/$workers/" "$(dirname "$0")/nginx.conf"
