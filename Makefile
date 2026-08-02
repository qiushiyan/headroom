BIN := $(HOME)/.local/bin/headroom

.PHONY: build install test vet fmt check test-pty

build:
	go build ./...

install:
	go build -o $(BIN) ./cmd/headroom

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: vet test

# Interactive-surface coverage go test can't reach; see DESIGN.md § Verification.
test-pty:
	./test/pty/run.sh
