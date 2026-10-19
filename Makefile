.PHONY: test test-linux build railgun origin bench fmt vet race lint

test:
	go test ./...

race:
	go test -race ./...

# Run the suite ON Linux, not merely cross-compiled for it.
#
# This is not redundant with `go test`. Several paths in this repository do not
# exist on darwin at all: splice(2) is Linux-only, so the splice arm of the
# body relay silently falls back to a userspace copy, and SO_REUSEPORT does not
# load balance on darwin or the BSDs, so a reuseport group behaves as a single
# listener. A local run therefore executes neither the zero-copy path nor the
# multi-listener accept path, and reports success for both.
#
# TestProxyForceCopyMatchesSplice is the clearest case: on darwin both of its
# arms take the copy path, so it compares the copy against itself and passes
# for the wrong reason.
test-linux:
	@command -v docker >/dev/null || { echo "test-linux: docker not found; needs a container runtime (colima, OrbStack or Docker Desktop)"; exit 1; }
	@docker info >/dev/null 2>&1 || { echo "test-linux: docker is installed but the daemon is not responding; start it (e.g. 'colima start') and retry"; exit 1; }
	docker run --rm -v "$(PWD)":/src -w /src golang:1.25 go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

build: railgun origin bench

railgun:
	go build -o bin/railgun ./cmd/railgun

origin:
	go build -o bin/origin ./bench/cmd/origin

bench:
	go build -o bin/bench ./bench/cmd/bench

# E1 (spec §5.4): does the single upstream pool mutex contend? Timings and
# profiles run separately because profiling perturbs the timings. Findings are
# recorded in internal/upstream/E1.md.
E1_OUT ?= bench-out/e1

e1:
	mkdir -p $(E1_OUT)
	go test ./internal/upstream -run '^$$' -bench 'Acquire|MutexOnly|ColdDial' \
		-benchtime 2s -count 5 | tee $(E1_OUT)/timings.txt
	go test ./internal/upstream -run '^$$' -bench 'Contended/hold=1' \
		-benchtime 3s -o $(E1_OUT)/upstream.test \
		-mutexprofile $(E1_OUT)/mutex.out -blockprofile $(E1_OUT)/block.out
	@echo
	@echo "=== mutex profile ==="
	go tool pprof -text -nodecount=10 $(E1_OUT)/upstream.test $(E1_OUT)/mutex.out
	@echo
	@echo "=== block profile ==="
	go tool pprof -text -nodecount=10 $(E1_OUT)/upstream.test $(E1_OUT)/block.out
