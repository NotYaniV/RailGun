# rig-proxy runbook

Host preparation for `rig-proxy`. Everything here applies **identically to the
NGINX arm and the Railgun arm** — the host is prepared once and both proxies run
against the same kernel state. A setting applied to one arm only is a fabricated
result.

Nothing here is optional-by-default: the settings in §1–§4 change measurements
by more than the effect size the project is hunting (20–50µs added latency,
spec §4.2). §6 is the part that matters most — **assert the state, do not assume
it**, and record the assertions with the results.

`bench/setup-host.sh` provisions software. This document covers kernel and
hardware state, which is deliberately separate because it needs root, survives
reboots differently, and must be re-verified after every instance restart.

---

## 1. Sysctls

`/etc/sysctl.d/99-railgun-rig.conf`, then `sysctl --system`.

```conf
# --- accept path -----------------------------------------------------------
# nginx.conf asks for backlog=65535 and Go's listener takes somaxconn as its
# backlog. At the default 4096 both are silently clamped and the configured
# value is a lie.
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535

# --- NIC ingress -----------------------------------------------------------
# Packets queued between the NIC's softirq and the socket. Too small and drops
# appear as latency outliers that look like proxy jitter.
net.core.netdev_max_backlog = 250000
net.core.netdev_budget = 600

# --- socket buffers --------------------------------------------------------
# W2/W4 move 1 MiB bodies. Undersized socket buffers throttle the transfer and
# the result becomes a measurement of the buffer, not of the proxy.
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.ipv4.tcp_rmem = 4096 262144 16777216
net.ipv4.tcp_wmem = 4096 262144 16777216

# --- ephemeral ports -------------------------------------------------------
# W3 runs without keepalive, so the proxy opens a new upstream connection per
# request and every close leaves a TIME_WAIT bound to a local port. At the
# default range a sustained W3 exhausts ports in well under a minute and the run
# fails with EADDRNOTAVAIL, which looks like a proxy defect.
net.ipv4.ip_local_port_range = 1024 65535
# Outbound-only reuse of TIME_WAIT sockets. Safe (unlike the removed
# tcp_tw_recycle) and directly relevant to W3.
net.ipv4.tcp_tw_reuse = 1

# --- keepalive connection behaviour ----------------------------------------
# Without this, a keepalive connection that goes idle for one RTO has its
# congestion window reset to the initial value and the next response restarts
# slow start. On W1/W2 that shows up purely as tail latency and is invisible in
# the mean.
net.ipv4.tcp_slow_start_after_idle = 0

# --- file descriptors ------------------------------------------------------
fs.file-max = 2000000

# --- splice pipe buffers (see §5) ------------------------------------------
fs.pipe-max-size = 4194304
fs.pipe-user-pages-soft = 0
fs.pipe-user-pages-hard = 0

# --- perf ------------------------------------------------------------------
# Required for the §5.5 splice guard (perf stat -e syscalls:sys_enter_splice)
# and for any perf-based profiling on the rig.
kernel.perf_event_paranoid = -1
kernel.kptr_restrict = 0
```

## 2. conntrack must not be in the path

`nf_conntrack` adds a hash lookup and a per-connection table entry to every
packet, and when the table fills it **drops packets** — which appears in results
as sporadic multi-second tail latency.

```sh
# Must print nothing:
lsmod | grep -E 'nf_conntrack|iptables|nf_tables'
```

If it is loaded, something (docker, firewalld, ufw, a stray iptables rule)
pulled it in. Remove the cause rather than raising `nf_conntrack_max`:

```sh
systemctl disable --now firewalld ufw docker 2>/dev/null
iptables -F; iptables -X; iptables -t nat -F
modprobe -r nf_conntrack_netlink nf_conntrack 2>/dev/null
```

Access control belongs in the cloud security group, not in a host firewall. The
origin runs here on rig-proxy — NGINX reaches it over loopback and rig-load
reaches it across the network for the `--null-url` arm — so port 9001 **on this
host** stays restricted to rig-load's address (`setup-host.sh:56`).

## 3. CPU state — the largest single source of p99.9 noise

Idle-state exit latency and frequency ramping both land squarely in the tail
being measured.

```sh
# Performance governor, no downclocking mid-run.
cpupower frequency-set -g performance

# Disable deep C-states. A core waking from C6 costs tens of µs — the same
# order as the entire effect being measured.
cpupower idle-set -D 0

# Transparent hugepage defrag stalls allocation in-line. Go's heap growth
# during a run is exactly where this bites.
echo never > /sys/kernel/mm/transparent_hugepage/defrag
echo madvise > /sys/kernel/mm/transparent_hugepage/enabled
```

Optional, needs a reboot, and worth it if the tail is still noisy after §1–§4 —
add to the kernel command line, substituting the proxy core set:

```
nohz_full=8-31 rcu_nocbs=8-31 intel_idle.max_cstate=1 processor.max_cstate=1
```

This removes the scheduler tick and RCU callback work from the proxy cores.
Record whether it was applied; it changes results and must be constant across
compared runs.

## 4. IRQ affinity and core pinning

The rule from spec §4.3: **the proxy's cores and the NIC's IRQ cores are
disjoint, and the same core set is used for both proxies.**

Sharing them means a softirq storm preempts the proxy at exactly the moment
throughput peaks, which reads as proxy-side latency.

```sh
# irqbalance will undo any manual affinity. Stop it first.
systemctl disable --now irqbalance

IFACE=ens5                      # SR-IOV / ENA interface
# One core per NIC queue, starting at core 0.
cpu=0
for irq in $(grep "$IFACE" /proc/interrupts | cut -d: -f1 | tr -d ' '); do
  printf '%x' $((1 << cpu)) > /proc/irq/$irq/smp_affinity
  cpu=$((cpu + 1))
done
```

With `N` NIC queues on cores `0..N-1`, the proxy core set is `N..ncpu-1`:

```sh
# NGINX: worker_processes = |core set|, plus worker_cpu_affinity over that mask.
# Railgun:
taskset -c 8-31 ./railgun ...
```

Go needs no `GOMAXPROCS` override — `runtime.NumCPU` is derived from
`sched_getaffinity` (`runtime/os_linux.go:114`) and Go 1.25 is cgroup CPU-limit
aware (`runtime/cgroup_linux.go`), so a `taskset`-pinned process already sizes
itself correctly. Assert it anyway (§6); do not trust it silently.

Also raise the file-descriptor limit for whichever proxy is running — NGINX asks
for `worker_rlimit_nofile 200000` in its own config and Railgun has no
equivalent, so it must come from the unit file or the launcher:

```sh
ulimit -n 200000
```

## 5. Splice pipe buffers — a silent performance cliff

`splice(2)` in Go moves bytes through an intermediate pipe. `internal/poll`
keeps a `sync.Pool` of pipes and asks for a 1 MiB pipe buffer
(`maxSpliceSize`), because 1 MiB per `splice` call means one round trip instead
of sixteen.

The important detail: `newPipe` calls
`unix.Fcntl(fds[0], F_SETPIPE_SZ, maxSpliceSize)` and **discards the error**
(`internal/poll/splice_linux.go`). If the request is refused, the pipe keeps whatever
size it was allocated with and every large body takes many times the syscalls.
Nothing logs, nothing fails, the throughput number is just worse.

Two limits refuse it:

- `fs.pipe-max-size` — default 1048576, i.e. **exactly** the 1 MiB Go asks for.
  It fits by zero margin, so any image or distro that lowers it breaks splice
  sizing silently. Assert the value; do not assume the default.
- `fs.pipe-user-pages-soft` — default 16384 pages (64 MiB) of pipe buffer **per
  user**. Past it the kernel allocates new pipes at a single page
  (`pipe_bufs = 1` in `alloc_pipe_info`) for processes without
  `CAP_SYS_RESOURCE`, and `F_SETPIPE_SZ` back up to 1 MiB then fails too.
  `0` disables the limit.

### This is measured, not inferred

`internal/sysx.ProbeSplice` holds N pipes open at once and reports what the
kernel granted each one. On Linux 6.8.0 (aarch64), stock `fs.pipe-user-pages-soft`:

| Pipes held | Result |
|---|---|
| 1 | `available pipe=1024KiB (1/1 at 1024KiB)` |
| 8 | `available pipe=1024KiB (8/8 at 1024KiB)` |
| **64** | `DEGRADED smallest=64KiB (63/64 at 1024KiB)` |
| 128 | `DEGRADED smallest=8KiB (63/128 at 1024KiB)` |

**The cliff is the 64th concurrent large-body relay**, exactly where the
arithmetic puts it: 16384 pages × 4 KiB = 64 MiB, at 1 MiB per pipe = 63 pipes
before the 64th is refused. W4 operates straight through that point.

Setting `fs.pipe-user-pages-soft=0` and re-running the same unprivileged probe
gives `128/128 at 1024KiB`; restoring 16384 brings the cliff back. So the §1
setting is verified to fix it, not merely believed to.

One tempting non-fix: `CAP_SYS_RESOURCE` also exempts a process from the soft
limit — a privileged container shows 128/128 with the stock sysctl. Do not take
that route. Raising a sysctl on a dedicated benchmark host is a smaller change
than granting the proxy a capability that lets it bypass resource accounting
generally, and it also would not match how the proxy is expected to be deployed.

### Verifying on the rig

```sh
go test ./internal/sysx -run SpliceReport -v      # prints the table above for this host
```

`SpliceProbe.String()` is built to be one startup log line, so that once it is
wired into `cmd/railgun` a degraded rig announces itself before the run rather
than in the numbers afterwards. A `DEGRADED` line invalidates the splice arm.

Then confirm it empirically under load:

```sh
# During a W4 run: splice count must be proportional to large-body requests
# (the spec §5.5 guard), and the per-call size should be near 1 MiB.
perf stat -e syscalls:sys_enter_splice -p "$(pgrep -x railgun)" -- sleep 10
```

Splice count much higher than expected for the bytes moved means the pipe is
64 KiB and the sysctls did not take.

## 6. Assert before measuring

Run this on rig-proxy immediately before each run and store the output in the
result set. A run whose preflight was not recorded is not a result.

```sh
#!/bin/sh
# bench preflight — every line below is a value that silently invalidates a run.
set -e
echo "kernel:        $(uname -r)"
echo "nginx:         $(nginx -V 2>&1 | head -1)"
echo "somaxconn:     $(sysctl -n net.core.somaxconn)"
echo "syn_backlog:   $(sysctl -n net.ipv4.tcp_max_syn_backlog)"
echo "port_range:    $(sysctl -n net.ipv4.ip_local_port_range)"
echo "tw_reuse:      $(sysctl -n net.ipv4.tcp_tw_reuse)"
echo "ssr_after_idle:$(sysctl -n net.ipv4.tcp_slow_start_after_idle)"
echo "pipe_max:      $(sysctl -n fs.pipe-max-size)"
echo "pipe_soft:     $(sysctl -n fs.pipe-user-pages-soft)"
echo "governor:      $(cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor)"
echo "thp_defrag:    $(cat /sys/kernel/mm/transparent_hugepage/defrag)"
echo "conntrack:     $(lsmod | grep -c nf_conntrack) modules"
echo "irqbalance:    $(systemctl is-active irqbalance || true)"
echo "nofile:        $(ulimit -n)"
echo "cmdline:       $(cat /proc/cmdline)"
```

Three assertions must **fail the run**, not merely be logged:

1. `conntrack` module count is not 0.
2. NGINX `worker_processes` and Railgun `GOMAXPROCS` are not equal.
3. The proxy's CPU affinity mask intersects the NIC IRQ cores.

Each of these produces a plausible-looking result that is wrong, which is worse
than an obviously broken one.
