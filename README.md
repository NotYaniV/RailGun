# Railgun

A performance research project asking whether a modern Linux-first HTTP reverse
proxy can beat NGINX on requests/sec and p99 tail latency.

The deliverable is a number and an explanation, not a production proxy. If NGINX
wins, that result gets published with the same rigor.

## Status

**M0 — benchmark harness: complete.** Built and validated before any proxy code,
deliberately. Without a rig that produces a reproducible NGINX baseline with a
published noise floor, "we beat NGINX" is a blog post rather than a result. See
[bench/README.md](bench/README.md).

**M1 — Approach A proxy: the data plane is in and serves traffic.** One
goroutine per downstream connection over the Go runtime's netpoll, a
zero-allocation HTTP/1.1 parser, an fd-agnostic upstream pool, and a splice
relay with a forced-copy control arm.

What is NOT done, and what is therefore not yet claimable:

- No Railgun-vs-NGINX number has been taken. Nothing here has run on the rig.
- Experiments E1 (upstream pool contention) and E2 (splice vs forced copy) are
  wired to be measurable but have not been run, so the splice threshold in
  `DefaultOptions` is a guess and is labelled as one in the source.
- Hypothesis H1 — that Go's work-stealing scheduler beats NGINX's fixed
  accept-time worker affinity on tail latency under CPU-skewed load — is
  unproven. It is the project's central claim and it may well be false. The
  falsification condition is stated in advance in the design spec.

### Review status of internal/proxy/relay.go

Stated because "defects found" is easy to read as "defects therefore gone".

The relay has had two review passes. They produced six defects: three reachable
by a client, two of them capable of desynchronising message framing between the
proxy and a peer, and one a denial of service that exhausts the upstream
connection slab from a few hundred one-packet requests. All six are fixed and
each has a test confirmed to fail without its fix.

Six in one file is a density that predicts more rather than a bill of health. A
third pass is what the second pass earned, not what it made unnecessary. The
file is the newest code in the repository and is doing the hardest thing in it:
holding two connections, a parser, a pool and a buffer in agreement about where
one message ends and the next begins.

Until that settles, a green benchmark run should not be read as validating this
file. A proxy can post excellent numbers while silently mis-framing a fraction
of a percent of exchanges, and the benchmark would not notice — wrk2 sends
well-formed requests and never truncates one.

## Layout

```
cmd/railgun/        entrypoint, flags, GC tuning, admin/metrics endpoint
internal/httpx/     zero-alloc HTTP/1.1 parse + serialize. []byte only.
internal/proxy/     connection lifecycle, relay, splice/copy switch
internal/upstream/  connection pool, passive health. Opaque int tokens.
internal/sysx/      SO_REUSEPORT listeners, accept-queue drain
internal/config/    immutable snapshots behind an atomic pointer
internal/metrics/   per-P sharded histograms, sampled, merged at scrape
bench/              the harness: provisioning, nginx.conf, origin, runner
```

`httpx` and `upstream` must not import `net`, `bufio`, `os`, or
`golang.org/x/sys/unix`. This is enforced by a test, not by review. The reason
is milestone M3: the io_uring data plane replaces the goroutine-per-connection
model entirely, and those two packages are meant to move under it unchanged. A
single transitive import of the I/O model would mean rewriting the parser
instead of relinking it.

## Build and run

```sh
make build                      # bin/railgun, bin/origin, bin/bench
make test                       # go test ./...
make race                       # go test -race ./...
make test-linux                 # the same suite, executed on Linux in a container

bin/origin -addr 127.0.0.1:9001 &
bin/railgun -addr :8080 -config examples/routes.json -memlimit 512MiB
curl -H 'Host: app.test' http://127.0.0.1:8080/bytes/1048576 -o /dev/null
curl -s http://127.0.0.1:9090/metrics
```

Routing is exact-host, case-insensitive, port-stripped. See
[examples/routes.json](examples/routes.json).

### Flags that exist for the experiments, not for operators

- `-force-copy` — route every response body through a userspace buffer, never
  splice. This is the control arm of experiment E2, run paired against the
  splice arm in the same session on the same rig. It exists because "splice
  happened" and "splice won" are different claims: below some body size the
  copy is faster, since pushing bytes through the intermediate pipe costs two
  ring round-trips that a single read/write into a warm L1-resident buffer does
  not.
- `-splice-threshold` — the body size at or above which splice is used. The
  default is a guess pending E2's measured crossover.
- `-instrument` / `-sample-rate` — two `time.Now` calls cost 40-60ns per
  request, which at the target request rates is a measurable fraction of the
  thing being measured. Peak-RPS runs go with `-instrument=false`, and the
  startup line records which way it ran. A peak-RPS number taken with
  instrumentation on is not comparable to one taken with it off.
- `-memlimit` — resident memory is the declared sacrifice, bounded at 4x
  NGINX's RSS and enforced here rather than discovered afterwards. The bound is
  real: a working set larger than L3 pays for itself in cache misses at exactly
  the request rate the memory was spent to buy. Running without it prints a
  warning and the run is not publishable.

### macOS runs the code but not all of its paths

`make test` on a Mac is not the same suite. Two of the paths this project exists
to measure do not exist on darwin:

- `splice(2)` is Linux-only, so the zero-copy body relay silently falls back to
  a userspace copy. `TestProxyForceCopyMatchesSplice` compares the copy path
  against itself and passes for the wrong reason.
- `SO_REUSEPORT` permits multiple binds on darwin and the BSDs but does not load
  balance across them (FreeBSD needs `SO_REUSEPORT_LB`; darwin has no
  equivalent). A group of N listeners behaves as one, so the accept path in
  design spec 5.1 is not exercised at all, and the binary warns about this at
  startup.

`make test-linux` runs the same suite in a container and is the only place those
paths execute. Both suites pass on Linux 6.8.

## Requirements

Go 1.25+. macOS builds and runs for development, but produces no valid numbers:
`splice(2)` is Linux-only, so the userspace-bytes-copied counter reads low on
darwin for the wrong reason. Benchmarks require Linux (kernel 6.x), `wrk2`,
`h2load`, `perf`, and NGINX on a two-instance rig. See `bench/setup-host.sh`
and `bench/RUNBOOK.md`.
