# Railgun

A performance research project asking whether a modern Linux-first HTTP reverse
proxy can beat NGINX on requests/sec and p99 tail latency.

The deliverable is a number and an explanation, not a production proxy. If NGINX
wins, that result gets published with the same rigor.

## Status

M0 — benchmark harness. No proxy code exists yet, deliberately: the harness is
built and validated against NGINX before there is anything to compare it to.
Without a rig that produces a reproducible NGINX baseline, "we beat NGINX" is a
blog post rather than a result.

See [bench/README.md](bench/README.md) for the rig and how to run it.

## Requirements

Go 1.25+. Benchmarks require Linux (kernel 6.x), `wrk2`, `h2load`, `perf`, and
NGINX on a two-instance rig. See `bench/setup-host.sh`.
