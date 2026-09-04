.PHONY: test build railgun origin bench fmt vet race lint

test:
	go test ./...

race:
	go test -race ./...

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
