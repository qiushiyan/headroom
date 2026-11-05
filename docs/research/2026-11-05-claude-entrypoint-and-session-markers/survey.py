# Thirty days of one machine's Claude Code work by entrypoint: sessions and
# price-weighted tokens (input 1, 1h cache write 2, cache read 0.1, output 5;
# each message counted once by id and request id), and the part a headroom
# launch started — a session whose first line is within 15 s of a launch line
# in the same directory. Reads only.
#
#   python3 -I survey.py            (or: ssh <host> python3 -I - < survey.py)
import collections
import datetime
import glob
import json
import os
import time

CUT = time.time() - 30 * 86400
WEIGHT = {'input_tokens': 1.0, 'cache_creation_input_tokens': 2.0,
          'cache_read_input_tokens': 0.1, 'output_tokens': 5.0}


def epoch(stamp):
    return datetime.datetime.fromisoformat(stamp.replace('Z', '+00:00')).timestamp()


launches = []
log = os.path.expanduser('~/.claude-accounts/launches.jsonl')
for line in open(log) if os.path.exists(log) else []:
    try:
        r = json.loads(line)
    except ValueError:
        continue
    if r.get('vendor', 'claude') == 'claude' and r.get('at_ms', 0) / 1000 >= CUT:
        launches.append((r['at_ms'] / 1000, r.get('cwd')))

sessions = {}
for path in glob.glob(os.path.expanduser('~/.claude/projects') + '/**/*.jsonl', recursive=True):
    try:
        if os.path.getmtime(path) < CUT:
            continue
    except OSError:
        continue
    seen, ep, cwd, start, sid, weight = set(), None, None, None, None, 0.0
    for line in open(path, errors='replace'):
        try:
            d = json.loads(line)
        except ValueError:
            continue
        ep, cwd, sid = ep or d.get('entrypoint'), cwd or d.get('cwd'), sid or d.get('sessionId')
        if start is None and d.get('timestamp'):
            start = epoch(d['timestamp'])
        m = d.get('message') if isinstance(d.get('message'), dict) else None
        usage = m and m.get('usage')
        if not usage or (m.get('id'), d.get('requestId')) in seen:
            continue
        seen.add((m.get('id'), d.get('requestId')))
        weight += sum(WEIGHT[k] * (usage.get(k) or 0) for k in WEIGHT)
    s = sessions.setdefault(sid or path, {'ep': None, 'cwd': None, 'start': None, 'w': 0.0})
    s['w'] += weight
    s['ep'] = s['ep'] or ep
    if '/subagents/' not in path:
        s['cwd'], s['start'] = s['cwd'] or cwd, s['start'] or start

total, via, count, count_via = (collections.Counter() for _ in range(4))
for s in sessions.values():
    if not s['start'] or s['start'] < CUT:
        continue
    ep = s['ep'] or '?'
    total[ep] += s['w']
    count[ep] += 1
    if any(abs(s['start'] - t) < 15 and s['cwd'] == cwd for t, cwd in launches):
        via[ep] += s['w']
        count_via[ep] += 1
grand = sum(total.values()) or 1
print(f"claude launches in the window: {len(launches)}")
for ep, w in total.most_common():
    print(f"{ep:14s} {count[ep]:5d} sessions {w / 1e6:9.1f} M weighted {w / grand:4.0%}"
          f" | via headroom launch: {count_via[ep]:5d} sessions {via[ep] / 1e6:9.1f} M")
