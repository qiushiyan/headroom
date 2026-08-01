BIN := $(HOME)/.local/bin/headroom

.PHONY: build install test vet fmt check

build:
	go build ./...

install:
	go build -o $(BIN) ./cmd/headroom

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: vet test
