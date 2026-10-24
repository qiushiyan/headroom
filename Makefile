PREFIX ?= $(HOME)/.local
BIN := $(PREFIX)/bin/headroom

.PHONY: build install test vet fix fmt check golden test-pty

build:
	go build ./...

# Built beside the target and renamed into place: `go build -o $(BIN)` leaves
# a window where the installed path is a partially written file, and the shell
# launchers call this binary at every launch.
install:
	@mkdir -p "$(PREFIX)/bin"
	@tmp=$$(mktemp "$(BIN).XXXXXX"); trap 'rm -f "$$tmp"' EXIT; \
	go build -o "$$tmp" ./cmd/headroom && chmod 755 "$$tmp" && mv -f "$$tmp" "$(BIN)"

test:
	go test -race ./...

vet:
	go vet ./...

# Go 1.27 modernizers must leave nothing to rewrite.
fix:
	@out=$$(go fix -diff ./...); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

fmt:
	gofmt -l -w .

check: vet fix test
	@test -z "$$(gofmt -l cmd internal)"

# Rewrite the golden help pages after an intended change; review the diff.
golden:
	CLIKIT_UPDATE=1 go test ./cmd/headroom -run TestHelpPages

# Interactive-surface coverage go test can't reach; see DESIGN.md § Verification.
test-pty:
	./test/pty/run.sh
