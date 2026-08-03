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
# One account carries an accountUuid (every real dir on this machine does —
# it is what the request ledger keys on) and one deliberately does not, so the
# degraded dir-name path stays exercised too.
mkdir -p "$HEADROOM_ACCOUNTS_ROOT/a@x.com" "$HEADROOM_ACCOUNTS_ROOT/b@x.com"
printf '{"oauthAccount":{"emailAddress":"a@x.com","accountUuid":"11111111-aaaa-4bbb-8ccc-dddddddddddd"}}' \
    >"$HEADROOM_ACCOUNTS_ROOT/a@x.com/.claude.json"
printf '{"oauthAccount":{"emailAddress":"b@x.com"}}' \
    >"$HEADROOM_ACCOUNTS_ROOT/b@x.com/.claude.json"
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
    # Each case starts from an unclaimed budget, or the board would open
    # inside a quiet period the previous case paid for.
    rm -f "$HEADROOM_ACCOUNTS_ROOT/state.json"
    if expect -f "$here/$1.exp" >"$work/$1.log" 2>&1; then
        echo "ok   $1"
    else
        echo "FAIL $1"
        sed 's/^/     /' "$work/$1.log"
        fail=1
    fi
}

# The non-interactive surfaces run at all (a dispatch regression once
# panicked on the bare invocation and only live use caught it). Off a
# terminal the bare invocation prints the board once instead of opening the
# picker — deleting the dashboard *command* must not delete the rendering.
if ! "$HEADROOM_BIN" >/dev/null 2>&1; then
    echo "FAIL board: bare invocation off a terminal failed"
    fail=1
else
    echo "ok   board"
fi
if ! "$HEADROOM_BIN" --json >/dev/null 2>&1; then
    echo "FAIL json: --json invocation failed"
    fail=1
else
    echo "ok   json"
fi

# Arrows + enter select the second account and write state.
run accounts_write
if [ "$(cat "$HEADROOM_ACCOUNTS_ROOT/.current" 2>/dev/null)" != "a@x.com" ]; then
    echo "FAIL accounts_write: .current not written with a@x.com"
    fail=1
fi

# ESC cancels and writes nothing.
run accounts_cancel
if [ -e "$HEADROOM_ACCOUNTS_ROOT/.current" ]; then
    echo "FAIL accounts_cancel: cancel must write nothing"
    fail=1
fi

# The board redraws its countdown, survives a burst of refresh keys, and quits.
run accounts_refresh

# SIGTERM mid-session must leave the terminal in canonical echoing mode.
run accounts_sigterm
if ! grep -qE '(^|[[:space:]])icanon' "$STTY_OUT" 2>/dev/null ||
    grep -qE '(^|[[:space:]])-icanon' "$STTY_OUT" 2>/dev/null ||
    grep -qE '(^|[[:space:]])-echo([[:space:]]|$)' "$STTY_OUT" 2>/dev/null; then
    echo "FAIL accounts_sigterm: terminal left raw after SIGTERM"
    cat "$STTY_OUT" 2>/dev/null || true
    fail=1
fi

# The session picker lists the fixture store; --json needs no terminal.
if ! "$HEADROOM_BIN" sessions --json | grep -q "$sid"; then
    echo "FAIL sessions-json: fixture session missing from listing"
    fail=1
else
    echo "ok   sessions-json"
fi

# The retired spelling: exit 2, actionable stderr, and nothing on stdout a
# positional reader could consume — a stale shell function is exactly who
# still calls it.
tomb_out=$("$HEADROOM_BIN" resume 2>/dev/null) && tomb_code=0 || tomb_code=$?
if [ "$tomb_code" != 2 ] || [ -n "$tomb_out" ]; then
    echo "FAIL resume-tombstone: exit $tomb_code stdout '$tomb_out', want 2 and empty"
    fail=1
else
    echo "ok   resume-tombstone"
fi

# Enter under the harness's re-pointed HEADROOM_HOME refuses into the picker
# (the fixture session's owner is the primary), and the advisory cd file —
# created at flag parse — stays empty: empty is the shell's "do not cd".
run resume_write
if [ ! -e "$RESUME_OUT" ] || [ -s "$RESUME_OUT" ]; then
    echo "FAIL resume_write: cd file missing or non-empty after a refusal"
    fail=1
fi

# q cancels: the cd file stays empty, so the shell does not cd.
run resume_cancel
run resume_preview
if [ -s "$RESUME_OUT" ]; then
    echo "FAIL resume_cancel: cancel must leave the cd file empty"
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
