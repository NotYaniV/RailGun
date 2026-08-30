#!/usr/bin/env bash
# Provision a Railgun benchmark host.
#
#   ./setup-host.sh proxy    # rig-proxy: nginx, perf, the origin server
#   ./setup-host.sh load     # rig-load: wrk2, h2load
#
# Requires Ubuntu 24.04 or Amazon Linux 2023 with kernel 6.x. The kernel version
# matters: io_uring multishot and provided buffer rings (spec §6) are 6.x
# features, and M3 cannot run without them.
set -euo pipefail

role="${1:-}"
[[ "$role" == "proxy" || "$role" == "load" ]] || {
    echo "usage: $0 <proxy|load>" >&2
    exit 2
}

kernel_major="$(uname -r | cut -d. -f1)"
if (( kernel_major < 6 )); then
    echo "WARNING: kernel $(uname -r) is older than 6.x; M3 io_uring work will not run here" >&2
fi

sudo apt-get update -y
sudo apt-get install -y build-essential git linux-tools-common "linux-tools-$(uname -r)"

# Raise limits on both roles. A benchmark that hits the default 1024 fd limit
# measures the limit rather than the proxy.
sudo tee /etc/security/limits.d/99-railgun.conf >/dev/null <<'LIMITS'
*  soft  nofile  1000000
*  hard  nofile  1000000
LIMITS

sudo sysctl -w net.core.somaxconn=65535
sudo sysctl -w net.ipv4.tcp_max_syn_backlog=65535
sudo sysctl -w net.ipv4.ip_local_port_range="1024 65535"
# tcp_tw_reuse matters for W3, which churns connections fast enough to exhaust
# ephemeral ports otherwise.
sudo sysctl -w net.ipv4.tcp_tw_reuse=1

if [[ "$role" == "proxy" ]]; then
    sudo apt-get install -y nginx
    sudo cp "$(dirname "$0")/nginx/nginx.conf" /etc/nginx/nginx.conf
    sudo nginx -t
    echo "Now build and start the origin server: make origin && ./bin/origin -addr 127.0.0.1:9001"
else
    # wrk2 is not packaged; it must be built from source.
    if ! command -v wrk2 >/dev/null; then
        sudo apt-get install -y libssl-dev zlib1g-dev
        git clone --depth 1 https://github.com/giltene/wrk2.git /tmp/wrk2
        make -C /tmp/wrk2
        sudo install /tmp/wrk2/wrk /usr/local/bin/wrk2
    fi
    sudo apt-get install -y nghttp2-client   # provides h2load
fi

echo "$role host ready. Kernel $(uname -r)."
