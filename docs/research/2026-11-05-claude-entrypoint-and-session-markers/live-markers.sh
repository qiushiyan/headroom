#!/bin/bash
# live-markers.sh — every running claude on this machine: print mode or
# interactive, and which session markers its environment inherited. Reads only.
for pid in $(pgrep -x claude; pgrep -f 'claude/versions/[0-9.]+$'); do
  line=$(ps -E -p "$pid" -o command= 2> /dev/null) || continue
  mode=interactive
  case " $line " in *" -p "* | *" --print "*) mode=print ;; esac
  marks=$(printf '%s' "$line" | grep -oE '(CLAUDE_CODE_CHILD_SESSION|CLAUDE_CODE_ENTRYPOINT)=[^ ]*' | sort -u | tr '\n' ' ')
  echo "$pid $mode tty=$(ps -p "$pid" -o tty= | tr -d ' ') ${marks:-no markers}"
done | sort -u
