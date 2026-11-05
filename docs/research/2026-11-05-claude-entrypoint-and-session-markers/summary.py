# One line per arm: the label each request carried (User-Agent, then the
# billing line's cc_entrypoint), whether a transcript and a sessions/<pid>.json
# were written, the effort requested, connections to the ancestor's messaging
# socket, and any warning about an inherited variable.
#
#   python3 -I summary.py <out-dir>
import glob
import json
import os
import re
import sys

out = sys.argv[1]
caps = open(os.path.join(out, 'cap.jsonl')).read().splitlines()


def plain(path):
    if not os.path.exists(path):
        return ''
    text = open(path, 'rb').read().decode('utf-8', 'replace')
    text = re.sub(r'\x1b\[[0-9;?<>=]*[A-Za-z]|\x1b\][^\x07]*\x07|\x1b[()][0-9A-Za-z]|\x1b.', '', text)
    return ' '.join(text.split())


for d in sorted(glob.glob(os.path.join(out, 'arms', '*')), key=os.path.getmtime):
    a, b = int(open(d + '/cap-start').read()), int(open(d + '/cap-end').read())
    ua, billing, effort = set(), set(), set()
    for line in caps[a:b]:
        r = json.loads(line)
        if not r['path'].startswith('/v1/messages') or 'count_tokens' in r['path']:
            continue
        m = re.search(r'\(external, ([^)]*)\)', r['headers'].get('user-agent', ''))
        ua.add(m.group(1) if m else '?')
        for s in r.get('system_heads') or []:
            m = re.search(r'cc_entrypoint=([^;]*)', s)
            if m:
                billing.add(m.group(1))
        effort.update(r.get('effort_hits') or [])
    transcripts = []
    for p in glob.glob(d + '/cfg/projects/*/*.jsonl'):
        for line in open(p):
            if '"entrypoint"' in line:
                transcripts.append(json.loads(line).get('entrypoint'))
                break
    registry = re.findall(r'"entrypoint"\s*:\s*"([^"]+)"', open(d + '/registry.json').read()) if os.path.exists(d + '/registry.json') else []
    sock = open(d + '/sock.log').read().count('connect')
    warn = re.findall(r'⚠[^❯─]{0,80}', plain(d + '/screen.bin'))
    debug = [l.split('] ', 1)[-1].strip() for l in open(d + '/debug.log', errors='replace')
             if 'ON_REQUESTS' in l] if os.path.exists(d + '/debug.log') else []
    print(f"{os.path.basename(d):14s} {open(d + '/env').read().strip() or '(clean)'}")
    print(f"    label={'/'.join(sorted(ua))} billing={'/'.join(sorted(billing))}"
          f" transcript={transcripts[0] if transcripts else 'NONE'} registry={registry[0] if registry else 'NONE'}"
          f" effort={'/'.join(sorted(effort))} socket={sock} exit={open(d + '/exit').read().strip()}")
    for w in (warn[:1] + debug[:1]):
        print(f"    {w}")
