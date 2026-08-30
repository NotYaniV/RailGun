# Dry-run image for validating the Railgun harness.
#
# This is NOT the benchmark rig. Everything runs in one container on a shared
# kernel over loopback, so any number it produces is meaningless. Its only job
# is to execute the harness end to end — real wrk2, the real nginx.conf with
# epoll, the real Lua scripts, real /proc sampling — so that defects are found
# here instead of during a paid two-VM session.
FROM golang:1.25-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
        nginx \
        build-essential git libssl-dev zlib1g-dev \
        procps curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# wrk2 is not packaged anywhere; it must be built from source. Pinned to a
# commit so a dry run is reproducible rather than tracking upstream HEAD.
RUN git clone https://github.com/giltene/wrk2.git /tmp/wrk2 \
    && cd /tmp/wrk2 \
    && git checkout 44a94c17d8e6a0bac8559b53da76848e430cb7a7 \
    && make -j"$(nproc)" \
    && install /tmp/wrk2/wrk /usr/local/bin/wrk2 \
    && rm -rf /tmp/wrk2

WORKDIR /src
COPY . .
RUN go build -o /usr/local/bin/origin ./bench/cmd/origin \
    && go build -o /usr/local/bin/bench  ./bench/cmd/bench

CMD ["bash", "/src/bench/dryrun/run.sh"]
