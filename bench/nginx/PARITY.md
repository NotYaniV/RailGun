# NGINX baseline parity review

Review of `bench/nginx/nginx.conf` against spec §4.1 (rig topology), §4.3
(pinning), §4.4 (workloads) and §5.5 (copy path).

The bar: any setting where NGINX is handicapped relative to Railgun makes a
Railgun win meaningless.

Status: the two P0s below are **fixed** in `nginx.conf`; this file is the record
of what changed and why. A third finding in the first draft of this document was
wrong and is withdrawn in §0.

---

## 0. Withdrawn: "the upstream address is loopback"

The first draft called `server 127.0.0.1:9001;` a P0 on the grounds that spec
§4.1 puts the origin servers on rig-load, so the address would resolve to
nothing on the real rig. **That finding was wrong.** The scripts are
unambiguous and they are authoritative:

- `setup-host.sh` provisions "rig-proxy: nginx, perf, **the origin server**".
- `bench/README.md` passes `--proxy-url http://10.0.0.5:8080` and
  `--null-url http://10.0.0.5:9001` — the same host.

The origin runs on rig-proxy, and the loopback upstream is deliberate. It is
also what makes the noise floor subtractable: the proxied path and the null path
each cross the network exactly once, so the delta between them is proxy cost and
nothing else. Had the origin been on rig-load, the proxied path would cross the
network twice against the null path's once and the difference would be dominated
by the extra traversal — precisely the noise §4.2 warns about.

Two things do follow from it, and both belong in the writeup rather than being
left for a reader to notice:

1. **Spec §4.1 is stale.** Its line "rig-load — runs the load generator and the
   origin servers" contradicts both scripts. The spec needs the correction, not
   the config.
2. **The upstream leg never touches a NIC.** Loopback means MTU 65536, no
   driver, no IRQ, no ENA. This flatters the upstream side of *both* proxies
   equally, so the comparison stays fair — but it flatters splice specifically,
   because splice's win grows with segment size. E2's measured crossover body
   size (§5.5) is therefore a loopback crossover, and it must be reported as
   one. A real network upstream would move it.

## P0 — fixed

### 1. Large responses were spilling to disk, so W2 and W4 measured the filesystem

Defaults previously in play, none of them overridden:

| Directive | Default | Consequence |
|---|---|---|
| `proxy_buffer_size` | 4k/8k | first-chunk buffer |
| `proxy_buffers` | 8 × 4k/8k (~32-64 KiB) | total in-memory response buffer |
| `proxy_max_temp_file_size` | 1024m | overflow goes to `proxy_temp_path` **on disk** |

W2 serves 1 MiB responses (`workload.go`, `/bytes/1048576`) and W4 is the
large-body arm. With `proxy_buffering on` and ~32 KiB of buffers, every one of
those responses overflowed and NGINX wrote it to a temp file. The baseline then
included a disk write and read that Railgun's splice relay never performs. That
is not NGINX's best showing — it is NGINX at its worst, and a win against it
proves nothing. This is the finding that mattered most: it sat directly under
the workload the entire splice story rests on.

Applied:

```nginx
proxy_buffering on;          # kept: buffered is NGINX's best showing
proxy_buffer_size 16k;
proxy_buffers 8 32k;         # 256 KiB in memory
proxy_max_temp_file_size 0;  # never spool to disk; stream past the buffers
```

`0` is the streaming behaviour and the closest analogue to Railgun's relay. If
Railgun's per-connection buffer budget settles far from 256 KiB, revisit
`proxy_buffers` so the two are in the same ballpark, and record both numbers
with the results.

### 2. NGINX was not pinned to the same core set as Railgun

Spec §4.3: *"Railgun/NGINX pinned to a fixed core set on rig-proxy; the same set
for both."* The config had `worker_processes auto` and no affinity directive.

`auto` resolves via `sysconf(_SC_NPROCESSORS_ONLN)`, which **ignores the CPU
affinity mask**. So the moment the runbook pins the proxy off the NIC IRQ cores
with `taskset`, NGINX still spawns a worker per host core while Railgun gets Ps
only for its allowed cores. The comparison silently becomes N cores vs M.

Applied: `worker_processes` is now `__WORKER_PROCESSES__`, substituted by
`bench/nginx/render.sh`, plus `worker_cpu_affinity auto` — which unlike
`worker_processes auto` *does* honour the inherited mask and binds one worker
per allowed core. `setup-host.sh` renders it (`NGINX_WORKERS` env, default
`nproc`) and `bench/dryrun/run.sh` renders it too, so no call site copies the
template unrendered and `nginx -t` never sees a placeholder.

Go needs no equivalent fix: `runtime.NumCPU` derives from `sched_getaffinity`
(`runtime/os_linux.go:114`) and Go 1.25 is cgroup CPU-limit aware
(`runtime/cgroup_linux.go`), so a `taskset`-pinned Railgun already sizes itself
correctly. The harness must still **assert** `GOMAXPROCS == worker_processes`
and fail the run on mismatch (RUNBOOK §6). A silent core-count mismatch is the
easiest way to publish a fake win.

### 3. Also applied while in the file

- `gzip off;` — off by default and Railgun does not compress, but stated so it
  cannot drift.
- `proxy_next_upstream error timeout;` and the two 60s timeouts — these *are*
  the defaults. Made visible because they are load-bearing for parity (see P1
  below), not because they change behaviour.

## P1 — affects interpretation, fix before the writeup

**4. `keepalive 512` is per worker, not global.** NGINX's upstream keepalive
cache is per worker process, so the real ceiling is 512 × `worker_processes`.
Railgun's `internal/upstream.Pool` cap is per process. State both in the same
units in the results, or one side looks like it has 32× the connection reuse.

**5. `backlog=65535` is clamped by `net.core.somaxconn`.** Default 4096 on
modern kernels. Go's listener takes somaxconn as its backlog too, so both are
clamped identically today — but the config *reads* as tuned when it is not.
Runbook must raise somaxconn, and the effective value should be asserted at run
time, not assumed.

**6. `worker_rlimit_nofile 200000` has no Railgun counterpart.** The Railgun
process needs the same `RLIMIT_NOFILE` from its unit file or launcher.

**7. `proxy_buffering on` is a semantic difference from Railgun's relay, not
just a tuning knob.** Buffered NGINX reads the whole response from the upstream
and releases the upstream connection before the client has finished reading;
Railgun's splice relay holds both. Under a slow client these behave differently
in upstream-connection demand. Recommendation: keep `on` (it is NGINX's best
showing) with finding #2 applied, state the difference explicitly in the
writeup, and run W4 once with `proxy_buffering off` as the like-for-like
streaming arm.

**8. NGINX silently retries the next upstream, so Railgun must match.** Now
explicit in the config as `proxy_next_upstream error timeout`. A Railgun that
does not retry will report errors where NGINX reports none, and the error-rate
column becomes meaningless. `internal/upstream.Acquire` returns `reused`
specifically so the policy can be implemented on the one case where it is safe:
retry once on a fresh connection when the failure was on a pooled connection and
the method is idempotent. Record the equivalence in the writeup; it is a stated
parity decision, not an implementation detail.

## P2 — noted, no action needed

- `sendfile on` / `tcp_nopush on` are inert on a pure proxy path (no static
  files are served). Harmless; keeping them costs nothing.
- `multi_accept on` is non-default. With `reuseport` it drains the whole accept
  queue per event-loop pass. Railgun's accept loop should do the same
  (accept until `EAGAIN`) so the accept strategy matches.
- `proxy_connect_timeout` / `proxy_read_timeout` are 60s, now stated in the
  config. Railgun must use the same values rather than its own.
- Record `nginx -V` (version, modules, TLS library) into every result set. A
  baseline without a recorded build is not reproducible.
