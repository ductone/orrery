.PHONY: build test check

build:
	go build -o orrery ./cmd/orrery

test:
	go test -race ./...

check: test
	go vet ./...
	buf lint
