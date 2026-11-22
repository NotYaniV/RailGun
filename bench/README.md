# Benchmark harness

M0 of Railgun. This exists before any proxy code, on purpose: without a rig that
produces a reproducible NGINX number, "we beat NGINX" is a blog post rather than
a result.

## Rig

Two cloud instances in the same cluster placement group, `.metal` or
whole-socket so no core is shared with a neighbour, kernel 6.x:

- **rig-proxy** — NGINX (or later Railgun) plus the origin server. One at a time.
- **rig-load** — `wrk2`, `h2load`, and the `bench` runner.

```bash
# on each host
./bench/setup-host.sh proxy      # or: load
```

## Running

```bash
# on rig-load
make build
./bin/bench run \
  --proxy-host rig-proxy \
  --proxy-url  http://10.0.0.5:8080 \
  --null-url   http://10.0.0.5:9001 \
  --arm nginx --repeats 5
./bin/bench report
```

`--null-url` points straight at the origin with no proxy in the path. It is not
optional: every result is reported as a delta over that noise floor, and the
report refuses to render without it.

For that to work the origin must bind `0.0.0.0:9001`, not `127.0.0.1:9001` —
NGINX reaches it over loopback, but the null arm is driven from rig-load and
needs it reachable across the network. Restrict port 9001 to rig-load in the
security group. The runner preflights both URLs before starting, so a
misconfiguration fails in seconds rather than after a full session.

## Reading a result

A verdict of `below-noise-floor` or `below-run-variance` means the rig could not
resolve the effect. That is not a win, and it must not be quoted as one. On a
virtualised NIC the p99.9 jitter floor is routinely 50–200µs while proxy
overhead differences are 20–50µs, so expect this verdict often and treat it as
the harness working correctly.

## Stress / robustness suite

`bin/stress` (`make stress`) is a portable, stdlib-only suite for a *running*
proxy: concurrency and object-size sweeps, open-loop rate steps, connection
churn, pipelining and mixed-size framing checks with every body length
verified, client aborts with leak checks, a soak with RSS sampling, slowloris /
idle / slow-reader hostile clients, and 21 malformed or smuggling requests.
It is not a performance claim against NGINX; that is still the rig's job.

```bash
./bin/stress -proxy 127.0.0.1:8080 -admin 127.0.0.1:9090 \
  -pid $(pgrep -x railgun) -out stress-$(uname -s).json   # add -quick for a smoke run
```

Use the same flags on macOS and Linux and diff the JSON. `-admin` and `-pid`
are optional. Exit status is non-zero if any objective check (framing, errors,
leaks, starvation) fails; throughput rows are INFO. Raise `ulimit -n` first.
