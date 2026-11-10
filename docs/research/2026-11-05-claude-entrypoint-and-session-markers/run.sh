#!/bin/bash
# run.sh [claude-binary] — both matrices of the record: which label a session
# sends, and what each variable a session leaves on its children does to a
# claude started under it, in print mode and interactive. About five minutes;
# prints summary.py's table and leaves the arms in the directory it names.
set -u
here=$(cd "$(dirname "$0")" && pwd)
export CLAUDE_BIN=${1:-$(python3 -I -c 'import os, shutil; print(os.path.realpath(shutil.which("claude")))')}
# Short on purpose: the messaging-socket arm's path must fit in sun_path.
# Resolved, because Claude Code matches folder trust against the real path
# (/private/tmp on macOS), and an arm whose trust names /tmp stops at the
# trust question.
OUT=$(cd "$(mktemp -d /tmp/hr-spike.XXXXXX)" && pwd -P)
export OUT
export CAP_PORT=${CAP_PORT:-18777}
: > "$OUT/cap.jsonl"
CAP_DELAY=4 python3 -I "$here/capture.py" "$CAP_PORT" "$OUT/cap.jsonl" &
capture=$!
python3 -I "$here/sockwatch.py" "$OUT/ancestor.sock" "$OUT/sock.log" &
watch=$!
sleep 900 &
ancestor=$!
trap 'kill $capture $watch $ancestor 2>/dev/null' EXIT
sleep 1
echo "claude: $CLAUDE_BIN ($("$CLAUDE_BIN" --version 2>/dev/null))  arms: $OUT"

for mode in p tui; do
  "$here/arm.sh" "${mode}-clean" $mode
  "$here/arm.sh" "${mode}-ep-cli" $mode CLAUDE_CODE_ENTRYPOINT=cli
  "$here/arm.sh" "${mode}-ep-sdk-cli" $mode CLAUDE_CODE_ENTRYPOINT=sdk-cli
  "$here/arm.sh" "${mode}-ep-sdk-ts" $mode CLAUDE_CODE_ENTRYPOINT=sdk-ts
  "$here/arm.sh" "${mode}-ep-custom" $mode CLAUDE_CODE_ENTRYPOINT=headroom
  "$here/arm.sh" "${mode}-child" $mode CLAUDE_CODE_CHILD_SESSION=1
  "$here/arm.sh" "${mode}-claudecode" $mode CLAUDECODE=1
  "$here/arm.sh" "${mode}-session-id" $mode CLAUDE_CODE_SESSION_ID=11111111-2222-3333-4444-555555555555
  "$here/arm.sh" "${mode}-attended" $mode CLAUDE_CODE_SESSION_ATTENDED=1
  "$here/arm.sh" "${mode}-pid" $mode CLAUDE_PID=$ancestor
  "$here/arm.sh" "${mode}-execpath" $mode CLAUDE_CODE_EXECPATH="$CLAUDE_BIN"
  "$here/arm.sh" "${mode}-messaging" $mode CLAUDE_CODE_MESSAGING_SOCKET="$OUT/ancestor.sock" CLAUDE_CODE_MESSAGING_TOKEN=spike
  "$here/arm.sh" "${mode}-effort" $mode CLAUDE_EFFORT=xhigh
done
"$here/arm.sh" p-on-requests p CLAUDE_CODE_ENTRYPOINT=sdk-cli CLAUDE_CODE_ENTRYPOINT_ON_REQUESTS=headroom

python3 -I "$here/summary.py" "$OUT"
