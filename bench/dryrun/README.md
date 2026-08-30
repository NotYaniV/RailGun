# Harness dry run

Validates the benchmark harness end to end in one Linux container, before any
cloud rig is provisioned.

**This produces no results.** Load generator, proxy and origin share a kernel
and talk over loopback; every number it prints is noise. What it validates is
the machinery: that nginx loads our config, that wrk2 runs our Lua scripts, that
the parsers accept real output rather than fixtures, that `/proc` sampling finds
the workers, and that the report renders.

The point is to find defects here rather than during a paid two-VM session.

## Requirements

A container runtime. Neither is installed by default on macOS:

```bash
brew install colima docker && colima start    # lightweight, CLI only
# or
brew install --cask orbstack                  # faster, includes a GUI
```

## Running

```bash
cd <repo root>
docker build -f bench/dryrun/Dockerfile -t railgun-dryrun .
docker run --rm railgun-dryrun
```

Override the defaults for a longer shakeout:

```bash
docker run --rm -e DURATION=30s -e REPEATS=5 railgun-dryrun
```

## What a pass looks like

- `nginx -t` succeeds — the config's `use epoll` and `reuseport` are valid
- both `127.0.0.1:9001` and `127.0.0.1:8080` respond
- `pgrep -x nginx` lists worker PIDs, so `/proc` sampling has targets
- every run parses; no "wrk2 output incomplete" errors
- the report renders with a noise-floor table and per-workload rows

Verdicts will mostly read `below-noise-floor`. On loopback with a shared kernel
that is the correct answer, and it is the harness working.
