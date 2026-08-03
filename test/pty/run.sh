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

# A session-store fixture for the resume surface. HEADROOM_HOME re-points
# the primary config dir and the projects store into the sandbox — without
# it, a resume test would list the real machine's transcripts and the
# delete test would delete one.
export HEADROOM_HOME="$work/home"
proj="$work/p1"
mkdir -p "$proj"
sid="11111111-2222-3333-4444-555555555555"
store="$HEADROOM_HOME/.claude/projects/$(printf '%s' "$proj" | tr '/.' '--')"
mkdir -p "$store"
cat >"$store/$sid.jsonl" <<EOF
{"type":"user","sessionId":"$sid","cwd":"$proj","message":{"role":"user","content":"hello fixture"}}
{"type":"ai-title","aiTitle":"fixture session","sessionId":"$sid"}
EOF
printf '{"display":"hello","sessionId":"%s","timestamp":1000}\n' "$sid" \
    >"$HEADROOM_HOME/.claude/history.jsonl"

# More sessions than one screen holds, all older than the first fixture (it
# must stay row 1 for resume_write), so resume_preview has a bottom entry to
# expand at the viewport's edge.
proj2="$work/p2"
mkdir -p "$proj2"
store2="$HEADROOM_HOME/.claude/projects/$(printf '%s' "$proj2" | tr '/.' '--')"
mkdir -p "$store2"
i=0
while [ "$i" -lt 14 ]; do
    fsid="00000000-0000-4000-8000-$(printf '%012d' "$i")"
    prompt="filler prompt $i"
    if [ "$i" -eq 13 ]; then
        prompt="bottom preview marker"
    fi
    cat >"$store2/$fsid.jsonl" <<EOF2
{"type":"user","sessionId":"$fsid","cwd":"$proj2","message":{"role":"user","content":"$prompt"}}
{"type":"ai-title","aiTitle":"filler $i","sessionId":"$fsid"}
EOF2
    touch -t "2026010112$(printf '%02d' $((30 - i)))" "$store2/$fsid.jsonl"
    i=$((i + 1))
done
export RESUME_OUT="$work/resume.out"

fail=0
run() {
    rm -f "$HEADROOM_ACCOUNTS_ROOT/.current" "$STTY_OUT" "$RESUME_OUT"
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

# The session picker lists the fixture store; --json needs no terminal.
if ! "$HEADROOM_BIN" resume --json | grep -q "$sid"; then
    echo "FAIL resume-json: fixture session missing from listing"
    fail=1
else
    echo "ok   resume-json"
fi

# Enter emits exactly one decision line on stdout: project dir, session id,
# empty config dir (the owner is the primary account).
run resume_write
if [ "$(cat "$RESUME_OUT" 2>/dev/null)" != "$(printf '%s\t%s\t' "$proj" "$sid")" ]; then
    echo "FAIL resume_write: decision line wrong:"
    cat "$RESUME_OUT" 2>/dev/null | sed 's/^/     /'
    fail=1
fi

# q cancels: nothing on stdout.
run resume_cancel
run resume_preview
if [ -s "$RESUME_OUT" ]; then
    echo "FAIL resume_cancel: cancel must write nothing to stdout"
    fail=1
fi

# SIGTERM inside the alt-screen session must still restore the terminal.
run resume_sigterm
if ! grep -qE '(^|[[:space:]])icanon' "$STTY_OUT" 2>/dev/null ||
    grep -qE '(^|[[:space:]])-icanon' "$STTY_OUT" 2>/dev/null ||
    grep -qE '(^|[[:space:]])-echo([[:space:]]|$)' "$STTY_OUT" 2>/dev/null; then
    echo "FAIL resume_sigterm: terminal left raw after SIGTERM"
    cat "$STTY_OUT" 2>/dev/null || true
    fail=1
fi

# dd + y removes the transcript — from the fixture store, proving the
# HEADROOM_HOME isolation that makes this test safe to run at all.
run resume_delete
if [ -e "$store/$sid.jsonl" ]; then
    echo "FAIL resume_delete: transcript still present"
    fail=1
fi

exit $fail
