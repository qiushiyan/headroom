#!/bin/bash
# arm.sh <name> <p|tui> [VAR=value ...] — one Claude Code session under a
# clean environment plus the given variables, against the stand-in on
# $CAP_PORT. run.sh sets OUT, CLAUDE_BIN and CAP_PORT.
#
# Nothing leaves the machine: a fake OAuth token, the API at 127.0.0.1, every
# other request sent to a dead proxy, and a fresh HOME and config dir that
# already trust the arm's working dir.
set -u
here=$(cd "$(dirname "$0")" && pwd)
name=$1 mode=$2
shift 2
d=$OUT/arms/$name
rm -rf "$d"
mkdir -p "$d/home" "$d/cfg" "$d/work" "$d/bin"
ln -s "$CLAUDE_BIN" "$d/bin/claude"
printf '{"hasCompletedOnboarding":true,"theme":"dark","projects":{"%s":{"hasTrustDialogAccepted":true,"hasCompletedProjectOnboarding":true}}}\n' "$d/work" > "$d/cfg/.claude.json"
printf '%s\n' "$*" > "$d/env"
wc -l < "$OUT/cap.jsonl" > "$d/cap-start"
: > "$OUT/sock.log"

base=(HOME="$d/home" PATH="$d/bin:/usr/bin:/bin" TERM=xterm-256color LANG=en_US.UTF-8
  CLAUDE_CONFIG_DIR="$d/cfg" ANTHROPIC_BASE_URL="http://127.0.0.1:$CAP_PORT"
  CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-spike-not-a-token
  HTTPS_PROXY=http://127.0.0.1:1 HTTP_PROXY=http://127.0.0.1:1 NO_PROXY=127.0.0.1,localhost)
args=(--model claude-opus-5-5 --debug-file "$d/debug.log")

cd "$d/work" || exit 1
if [ "$mode" = p ]; then
  env -i "${base[@]}" "$@" claude -p "say ok" "${args[@]}" > "$d/stdout" 2> "$d/stderr" < /dev/null &
  pid=$!
  sleep 3
  cat "$d"/cfg/sessions/*.json > "$d/registry.json" 2> /dev/null
  wait "$pid"
else
  env -i "${base[@]}" "$@" python3 -I "$here/drive_tui.py" "$d" "${args[@]}" > "$d/stdout" 2> "$d/stderr"
fi
echo $? > "$d/exit"
wc -l < "$OUT/cap.jsonl" > "$d/cap-end"
cp "$OUT/sock.log" "$d/sock.log"
