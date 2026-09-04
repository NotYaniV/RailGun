# Dry-run image for validating the Railgun harness.
#
# This is NOT the benchmark rig. Everything runs in one container on a shared
# kernel over loopback, so any number it produces is meaningless. Its only job
# is to execute the harness end to end — real wrk2, the real nginx.conf with
# epoll, the real Lua scripts, real /proc sampling — so that defects are found
# here instead of during a paid two-VM session.
# Pinned to amd64 even on an ARM host. wrk2 bundles a LuaJIT from ~2016 that
# predates ARM64 support and fails with "No support for this architecture
# (yet)"; wrk2 has been unmaintained since 2019, so this will not be fixed
# upstream. Emulation is slow, which costs nothing here: a dry run's numbers are
# meaningless by construction and only the machinery is under test.
#
# Pinned by the amd64-specific DIGEST rather than `--platform=linux/amd64`.
# Docker's legacy builder resolves a tag to one local image ID regardless of
# platform, so on an ARM host it silently reuses the arm64 image and its cached
# layers — the build then runs an arm64 gcc and LuaJIT fails exactly as if no
# platform had been requested. A digest names one platform's image and cannot
# collide. It also pins the toolchain, which a benchmark repo wants anyway.
#
# golang:1.25-bookworm, linux/amd64
FROM golang@sha256:c268a04d59aea0b180ed9946a658cfab9e7b3391dc90eed6e4969ccff98c851f

RUN apt-get update && apt-get install -y --no-install-recommends \
        nginx \
        build-essential git libssl-dev zlib1g-dev \
        procps curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# wrk2 is not packaged anywhere; it must be built from source. Pinned to a
# commit so a dry run is reproducible rather than tracking upstream HEAD.
#
# Built serially, not with `make -j`. Under qemu emulation on a small VM,
# parallel gcc invocations crash with "internal compiler error: Segmentation
# fault signal terminated program cc1". wrk2 itself is a dozen C files and
# LuaJIT dominates the time either way, so serialising costs little.
RUN git clone https://github.com/giltene/wrk2.git /tmp/wrk2 \
    && cd /tmp/wrk2 \
    && git checkout 44a94c17d8e6a0bac8559b53da76848e430cb7a7 \
    && make \
    && install /tmp/wrk2/wrk /usr/local/bin/wrk2 \
    && rm -rf /tmp/wrk2

WORKDIR /src
COPY . .
RUN go build -o /usr/local/bin/origin ./bench/cmd/origin \
    && go build -o /usr/local/bin/bench  ./bench/cmd/bench

CMD ["bash", "/src/bench/dryrun/run.sh"]
