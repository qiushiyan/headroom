#!/bin/sh
# PTY harness for the board — the behavior `go test` can't reach: real
# raw-mode terminal sessions, keys and quitting through a pty, and
# signal-time terminal restoration. Driven with expect(1) (ships with
# macOS); `make test-pty` runs it.
#
# Two patterns here are load-bearing:
#   - the foreground fixture launcher records its PID before exec; signal
#     only that child, keeping other headroom sessions untouched;
#   - terminal state after a signal death is inspected by an sh wrapper
#     that runs `stty -a` in the same pty once headroom is gone.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

HEADROOM_BIN="$work/headroom"
(cd "$repo" && go build -o "$HEADROOM_BIN" ./cmd/headroom)

# A fake accounts root with two extra accounts. The usage URL points at a
# closed local port: fetches fail instantly and no token leaves the machine,
# so the picker renders deterministic status lines with no network.
export HEADROOM_ACCOUNTS_ROOT="$work/accounts"
export HEADROOM_PRIMARY_NAME=primary
export HEADROOM_USAGE_URL="http://127.0.0.1:1/usage"
# The same for Codex, from the first invocation on: a shell that exports
# HEADROOM_CODEX_ACCOUNTS_ROOT would otherwise lend its real root — which
# HEADROOM_HOME does not override — to every case below, against the live
# usage URL. The fixture root does not exist until the two-vendor block
# creates it, so until then Codex is absent and every frame is the
# single-vendor one.
export HEADROOM_CODEX_ACCOUNTS_ROOT="$work/codex-accounts"
export HEADROOM_CODEX_USAGE_URL="http://127.0.0.1:1/usage"
unset HEADROOM_CODEX_PRIMARY_NAME HEADROOM_CODEX_LAUNCHER_FORMAT HEADROOM_LAUNCHER_FORMAT
unset CODEX_HOME CODEX_SQLITE_HOME CODEX_API_KEY CODEX_ACCESS_TOKEN
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
export HEADROOM_PID_FILE="$work/headroom.pid"

# HEADROOM_HOME re-points the primary config dir and the session store into
# the sandbox, so no case reads the real machine's sessions.
export HEADROOM_HOME="$work/home"
mkdir -p "$HEADROOM_HOME/.claude/projects"

# The exec-success fixture: an extra account with valid topology, and a stub
# claude that records the config dir and argv it received. Every case uses fixture auth and credential commands; a real
# terminal never requires real account credentials.
mkdir -p "$work/stub"
cat >"$work/stub/claude" <<'EOS'
#!/bin/sh
if [ "${1-}" = auth ]; then
    echo '{"loggedIn":false}'
else
    echo "STUB-CLAUDE cfg=${CLAUDE_CONFIG_DIR-unset} args=$*"
    printf 'STUB-TTY %s\n' "$(stty -g)"
fi
exit 0
EOS
cat >"$work/stub/security" <<'EOS'
#!/bin/sh
# The fixture has no Keychain items.
exit 44
EOS
chmod +x "$work/stub/claude" "$work/stub/security"
export PATH="$work/stub:$PATH"
export STUB_DIR="$work/stub"
ln -s "$HEADROOM_HOME/.claude/projects" "$HEADROOM_ACCOUNTS_ROOT/a@x.com/projects"

fail=0
run() {
    rm -f "$HEADROOM_ACCOUNTS_ROOT/.current" "$STTY_OUT" "$HEADROOM_PID_FILE"
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
# The compact layout reaches the non-interactive print too: off a terminal,
# `accounts --compact` writes the column header and one row per account —
# the interactive case alone would stay green with the one-shot path
# quietly printing blocks.
compact_out=$("$HEADROOM_BIN" accounts --compact 2>/dev/null)
if ! printf '%s\n' "$compact_out" | grep -q '^account' ||
    [ "$(printf '%s\n' "$compact_out" | grep -c '@x.com')" -ne 2 ]; then
    echo "FAIL compact-print: off a terminal, accounts --compact must print the header and one row per account"
    printf '%s\n' "$compact_out" | sed 's/^/     /'
    fail=1
else
    echo "ok   compact-print"
fi

# ESC closes the board and writes nothing.
run accounts_cancel
if [ -e "$HEADROOM_ACCOUNTS_ROOT/.current" ]; then
    echo "FAIL accounts_cancel: closing must write nothing"
    fail=1
fi

# The board redraws its countdown, survives a burst of refresh keys, and quits.
run accounts_refresh

# The compact layout draws its column header and one row per account and quits
# clean — the same loop under a second presentation.
run accounts_compact
if [ -e "$HEADROOM_ACCOUNTS_ROOT/.current" ]; then
    echo "FAIL accounts_compact: quit must write nothing"
    fail=1
fi

# The board marks the next account; enter and a, the keys that once recorded
# routing, write nothing.
run accounts_auto
if [ -e "$HEADROOM_ACCOUNTS_ROOT/.current" ]; then
    echo "FAIL accounts_auto: enter or a wrote .current ('$(cat "$HEADROOM_ACCOUNTS_ROOT/.current")')"
    fail=1
fi

# Automatic placement through the real binary. Four more accounts with a valid
# topology join a@x.com; the primary cannot be launched under a re-pointed
# HEADROOM_HOME and b@x.com has no sessions link, so both must be left out by
# name rather than chosen. The stub claude prints the config dir it was given.
for n in c d e f; do
    mkdir -p "$HEADROOM_ACCOUNTS_ROOT/$n@x.com"
    printf '{"oauthAccount":{"emailAddress":"%s@x.com","accountUuid":"2222222%s-aaaa-4bbb-8ccc-dddddddddddd"}}' "$n" "$n" \
        >"$HEADROOM_ACCOUNTS_ROOT/$n@x.com/.claude.json"
    ln -s "$HEADROOM_HOME/.claude/projects" "$HEADROOM_ACCOUNTS_ROOT/$n@x.com/projects"
done
rm -f "$HEADROOM_ACCOUNTS_ROOT/state.json" "$HEADROOM_ACCOUNTS_ROOT/launches.jsonl"

# A dry run changes nothing: no state file, no log.
dry_out=$("$HEADROOM_BIN" launch --dry-run 2>&1) && dry_code=0 || dry_code=$?
if [ "$dry_code" != 0 ] || ! printf '%s\n' "$dry_out" | grep -q 'would start claude on a@x.com' ||
    ! printf '%s\n' "$dry_out" | grep -q 'b@x.com.*excluded' ||
    [ -e "$HEADROOM_ACCOUNTS_ROOT/state.json" ] || [ -e "$HEADROOM_ACCOUNTS_ROOT/launches.jsonl" ]; then
    echo "FAIL launch-dry-run: exit $dry_code, or it left something behind"
    printf '%s\n' "$dry_out" | sed 's/^/     /'
    fail=1
else
    echo "ok   launch-dry-run"
fi

# Five launches started together land on the five launchable accounts: each
# takes the lock in turn and counts the ones before it.
i=0
while [ "$i" -lt 5 ]; do
    "$HEADROOM_BIN" launch -- -p "turn $i" >"$work/auto.$i.out" 2>"$work/auto.$i.err" &
    i=$((i + 1))
done
wait
spread=$(cat "$work"/auto.*.out | sed -n 's/^STUB-CLAUDE cfg=\([^ ]*\) .*/\1/p' | sort -u | wc -l | tr -d ' ')
if [ "$spread" != 5 ] || ! grep -q 'auto' "$work/auto.0.err"; then
    echo "FAIL launch-auto-spread: five launches started together reached $spread account(s)"
    cat "$work"/auto.*.out "$work"/auto.*.err | sed 's/^/     /'
    fail=1
else
    echo "ok   launch-auto-spread"
fi
# Every launch is one line in the log, whole, though they were appended
# together.
log="$HEADROOM_ACCOUNTS_ROOT/launches.jsonl"
if [ "$(grep -c '"mode":"auto"' "$log" 2>/dev/null)" != 5 ] || [ "$(wc -l <"$log" | tr -d ' ')" != 5 ]; then
    echo "FAIL launch-log: the log does not hold the five launches, one line each"
    sed 's/^/     /' "$log" 2>/dev/null
    fail=1
else
    echo "ok   launch-log"
fi
# A named session keeps its account across turns: the first turn's id is
# placed and recorded, and the second turn follows it wherever the load is.
auto_sid="aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
first=$("$HEADROOM_BIN" launch -- -p --session-id "$auto_sid" 2>/dev/null | sed -n 's/^STUB-CLAUDE cfg=\([^ ]*\) .*/\1/p')
second=$("$HEADROOM_BIN" launch -- -p --resume "$auto_sid" 2>"$work/auto.resume.err" | sed -n 's/^STUB-CLAUDE cfg=\([^ ]*\) .*/\1/p')
if [ -z "$first" ] || [ "$first" != "$second" ] || ! grep -q "this session's account" "$work/auto.resume.err"; then
    echo "FAIL launch-auto-session: first turn on '$first', second on '$second'"
    sed 's/^/     /' "$work/auto.resume.err"
    fail=1
else
    echo "ok   launch-auto-session"
fi
# The one-shot board marks one account's row.
auto_board=$("$HEADROOM_BIN" 2>/dev/null)
if [ "$(printf '%s\n' "$auto_board" | grep -c '@x\.com.*← next')" != 1 ]; then
    echo "FAIL board-auto: the board must mark one row"
    printf '%s\n' "$auto_board" | sed 's/^/     /'
    fail=1
else
    echo "ok   board-auto"
fi
# Back to the fixture every other case expects.
for n in c d e f; do rm -rf "$HEADROOM_ACCOUNTS_ROOT/$n@x.com"; done
rm -f "$HEADROOM_ACCOUNTS_ROOT/state.json" "$HEADROOM_ACCOUNTS_ROOT/launches.jsonl"

# Two vendors. Codex is absent from every other case — its directories do not
# exist under the fixture home, so those frames are what they always were —
# and present for this block alone, once its fixture root exists. The tokens
# are unsigned fixtures and the usage URL is a closed port.
b64() { printf '%s' "$1" | base64 | tr '+/' '-_' | tr -d '=\n'; }
jwt() { printf '%s.%s.sig' "$(b64 '{"alg":"none"}')" "$(b64 "$1")"; }
mkdir -p "$HEADROOM_CODEX_ACCOUNTS_ROOT/cx@x.com" "$HEADROOM_HOME/.codex/sessions"
ln -s "$HEADROOM_HOME/.codex/sessions" "$HEADROOM_CODEX_ACCOUNTS_ROOT/cx@x.com/sessions"
id_token=$(jwt '{"email":"cx@x.com","https://api.openai.com/auth":{"chatgpt_plan_type":"pro","chatgpt_account_id":"acct-cx","chatgpt_user_id":"user-cx"}}')
access_token=$(jwt '{"exp":4102444800}')
printf '{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":"%s","access_token":"%s","refresh_token":"rt","account_id":"acct-cx"},"last_refresh":"2026-09-13T08:00:00Z"}' \
    "$id_token" "$access_token" >"$HEADROOM_CODEX_ACCOUNTS_ROOT/cx@x.com/auth.json"

# Off a terminal, two vendors print under a heading each, and --json is still
# one document with a vendor on every account.
two_out=$("$HEADROOM_BIN" 2>/dev/null)
if ! printf '%s\n' "$two_out" | grep -q '^Claude Code$' || ! printf '%s\n' "$two_out" | grep -q '^Codex$' ||
    ! printf '%s\n' "$two_out" | grep -q 'cx@x.com'; then
    echo "FAIL two-vendor-print: want a heading per vendor and the Codex account"
    printf '%s\n' "$two_out" | sed 's/^/     /'
    fail=1
else
    echo "ok   two-vendor-print"
fi
two_json=$("$HEADROOM_BIN" --json 2>/dev/null)
if ! printf '%s' "$two_json" | grep -q '"schema": 8' || ! printf '%s' "$two_json" | grep -q '"vendor": "codex"'; then
    echo "FAIL two-vendor-json: want schema 8 and a vendor per account"
    fail=1
else
    echo "ok   two-vendor-json"
fi
one_out=$("$HEADROOM_BIN" accounts --vendor codex 2>/dev/null)
if printf '%s\n' "$one_out" | grep -q '^Codex$' || printf '%s\n' "$one_out" | grep -q 'a@x.com'; then
    echo "FAIL one-vendor-print: --vendor codex must print Codex alone, with no heading"
    fail=1
else
    echo "ok   one-vendor-print"
fi

# Tab switches pages; enter and a on the Codex page write neither vendor's
# .current.
rm -f "$HEADROOM_CODEX_ACCOUNTS_ROOT/.current" "$HEADROOM_CODEX_ACCOUNTS_ROOT/state.json"
run accounts_tab
if [ -e "$HEADROOM_CODEX_ACCOUNTS_ROOT/.current" ] || [ -e "$HEADROOM_ACCOUNTS_ROOT/.current" ]; then
    echo "FAIL accounts_tab: a key on the Codex page wrote a .current"
    fail=1
fi
# Codex goes absent again for everything below.
rm -rf "$HEADROOM_CODEX_ACCOUNTS_ROOT" "$HEADROOM_HOME/.codex"

# SIGTERM mid-session must leave the terminal in canonical echoing mode.
run accounts_sigterm
if ! grep -qE '(^|[[:space:]])icanon' "$STTY_OUT" 2>/dev/null ||
    grep -qE '(^|[[:space:]])-icanon' "$STTY_OUT" 2>/dev/null ||
    grep -qE '(^|[[:space:]])-echo([[:space:]]|$)' "$STTY_OUT" 2>/dev/null; then
    echo "FAIL accounts_sigterm: terminal left raw after SIGTERM"
    cat "$STTY_OUT" 2>/dev/null || true
    fail=1
fi
# …and with the cursor below the frame: the wrapper's marker must begin its
# own line (ANSI stripped — restore sequences precede it legitimately). The
# board's last line carries no newline, so only the close path's step-below
# puts it there.
esc=$(printf '\033')
if ! sed "s/${esc}\[[0-9;?]*[a-zA-Z]//g" "$work/accounts_sigterm.log" 2>/dev/null | grep -q '^NEWLINE-OK'; then
    echo "FAIL accounts_sigterm: signal exit left the cursor glued to the frame's last line"
    fail=1
fi

# The board must not duplicate itself into scrollback — the one behavior
# that needs a real scrollback buffer, so it runs under tmux and is skipped
# where tmux is absent (expect alone stays the harness's only requirement).
# A pane shorter than the board forces the redraw path that once scrolled a
# frame copy per second; a width round-trip forces the reflow-reset path.
# The emulator itself archives one pre-resize screen per resize, so after
# resizing the assertion is a bound, not zero.
if command -v tmux >/dev/null 2>&1; then
    rm -f "$HEADROOM_ACCOUNTS_ROOT/state.json"
    tmux_sock="$work/tmux.sock"
    tmx() { tmux -S "$tmux_sock" "$@"; }
    # The pane runs the picker directly (exec, no shell lingering), and every
    # phase re-checks that the pane still runs headroom: an early crash would
    # leave a static frame that satisfies any scrollback bound vacuously.
    board_alive() {
        [ "$(tmx display-message -t board -p '#{pane_current_command}' 2>/dev/null || true)" = headroom ]
    }
    tmx kill-server 2>/dev/null || true
    tmx new-session -d -x 100 -y 8 -s board "PATH='$STUB_DIR':\$PATH exec '$HEADROOM_BIN'"
    sleep 6
    tmx send-keys -t board j j k 2>/dev/null || true
    sleep 2
    hist=$(tmx display-message -t board -p '#{history_size}' 2>/dev/null || echo missing)
    if ! board_alive; then
        echo "FAIL scrollback: picker not running after the redraw interval"
        fail=1
    elif [ "$hist" != 0 ]; then
        echo "FAIL scrollback: $hist lines in scrollback after redraws in a short pane"
        tmx capture-pane -t board -p -S -50 | sed 's/^/     /'
        fail=1
    else
        tmx resize-window -t board -x 60 -y 8
        sleep 2
        tmx resize-window -t board -x 100 -y 8
        sleep 3
        hist=$(tmx display-message -t board -p '#{history_size}' 2>/dev/null || echo missing)
        if ! board_alive; then
            echo "FAIL scrollback: picker not running after the resize round-trip"
            fail=1
        elif [ "$hist" = missing ] || [ "$hist" -gt 40 ]; then
            echo "FAIL scrollback: $hist lines after a resize round-trip — growing per redraw, not per resize"
            fail=1
        else
            echo "ok   scrollback"
        fi
    fi
    tmx kill-server 2>/dev/null || true

else
    echo "skip scrollback (tmux not installed)"
fi

# A retired verb is an unknown word: exit 2 and nothing on stdout a
# positional reader could consume — a stale shell function is exactly who
# still calls one.
for verb in resume sessions resolve launches; do
    out=$("$HEADROOM_BIN" "$verb" 2>/dev/null) && code=0 || code=$?
    if [ "$code" != 2 ] || [ -n "$out" ]; then
        echo "FAIL retired-$verb: exit $code stdout '$out', want 2 and empty"
        fail=1
    else
        echo "ok   retired-$verb"
    fi
done

exit $fail
