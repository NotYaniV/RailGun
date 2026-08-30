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
