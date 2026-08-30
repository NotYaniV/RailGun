.PHONY: test build origin bench fmt vet

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

build: origin bench

origin:
	go build -o bin/origin ./bench/cmd/origin

bench:
	go build -o bin/bench ./bench/cmd/bench
