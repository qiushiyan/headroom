#!/bin/sh
# PTY harness for the picker — the behavior `go test` can't reach: real
# raw-mode terminal sessions, selection/cancel through a pty, and
# signal-time terminal restoration. Driven with expect(1) (ships with
# macOS); `make test-pty` runs it.
#
# Two patterns here are load-bearing:
#   - kill with `pkill -nx headroom`, never -f — -f would match the sh
#     wrapper too and tear the harness down;
#   - terminal state after a signal death is inspected by an sh wrapper
#     that runs `stty -a` in the same pty once headroom is gone.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The binary must be named exactly "headroom" for pkill -x to find it.
HEADROOM_BIN="$work/headroom"
(cd "$repo" && go build -o "$HEADROOM_BIN" ./cmd/headroom)

# A fake accounts root with two extra accounts. The usage URL points at a
# closed local port: fetches fail instantly and no token leaves the machine,
# so the picker renders deterministic status lines with no network.
export HEADROOM_ACCOUNTS_ROOT="$work/accounts"
export HEADROOM_PRIMARY_NAME=primary
export HEADROOM_USAGE_URL="http://127.0.0.1:1/usage"
for a in a@x.com b@x.com; do
    mkdir -p "$HEADROOM_ACCOUNTS_ROOT/$a"
    printf '{"oauthAccount":{"emailAddress":"%s"}}' "$a" \
        >"$HEADROOM_ACCOUNTS_ROOT/$a/.claude.json"
done
export HEADROOM_BIN
export STTY_OUT="$work/stty.out"

fail=0
run() {
    rm -f "$HEADROOM_ACCOUNTS_ROOT/.current" "$STTY_OUT"
    if expect -f "$here/$1.exp" >"$work/$1.log" 2>&1; then
        echo "ok   $1"
    else
        echo "FAIL $1"
        sed 's/^/     /' "$work/$1.log"
        fail=1
    fi
}

# The non-interactive surfaces run at all (a dispatch regression once
# panicked on the bare invocation and only live use caught it).
if ! "$HEADROOM_BIN" >/dev/null 2>&1; then
    echo "FAIL dashboard: bare invocation failed"
    fail=1
else
    echo "ok   dashboard"
fi
if ! "$HEADROOM_BIN" --json >/dev/null 2>&1; then
    echo "FAIL json: --json invocation failed"
    fail=1
else
    echo "ok   json"
fi

# Arrows + enter select the second account and write state.
run select_write
if [ "$(cat "$HEADROOM_ACCOUNTS_ROOT/.current" 2>/dev/null)" != "a@x.com" ]; then
    echo "FAIL select_write: .current not written with a@x.com"
    fail=1
fi

# ESC cancels and writes nothing.
run select_cancel
if [ -e "$HEADROOM_ACCOUNTS_ROOT/.current" ]; then
    echo "FAIL select_cancel: cancel must write nothing"
    fail=1
fi

# Watch draws, q quits with exit 0.
run watch_quit

# SIGTERM mid-session must leave the terminal in canonical echoing mode.
run select_sigterm
if ! grep -qE '(^|[[:space:]])icanon' "$STTY_OUT" 2>/dev/null ||
    grep -qE '(^|[[:space:]])-icanon' "$STTY_OUT" 2>/dev/null ||
    grep -qE '(^|[[:space:]])-echo([[:space:]]|$)' "$STTY_OUT" 2>/dev/null; then
    echo "FAIL select_sigterm: terminal left raw after SIGTERM"
    cat "$STTY_OUT" 2>/dev/null || true
    fail=1
fi

exit $fail
