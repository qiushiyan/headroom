BIN := $(HOME)/.local/bin/headroom

.PHONY: build install test vet fmt check test-pty

build:
	go build ./...

# Built beside the target and renamed into place: `go build -o $(BIN)` leaves
# a window where the installed path is a partially written file, and the shell
# launchers call this binary at every launch.
install:
	go build -o $(BIN).new ./cmd/headroom
	mv -f $(BIN).new $(BIN)

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
