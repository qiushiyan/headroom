# Runs interactive Claude Code in a pseudo-terminal: waits for its prompt,
# types one message, reads the session registry while the reply is held,
# then ends the session. Its screen is kept as screen.bin in the arm's dir.
#
#   python3 -I drive_tui.py <arm-dir> [claude args...]
import glob
import os
import pty
import select
import signal
import sys
import time

arm = sys.argv[1]
pid, fd = pty.fork()
if pid == 0:
    os.execvp('claude', ['claude'] + sys.argv[2:])

screen = b''


def pump(seconds):
    global screen
    end = time.time() + seconds
    while time.time() < end:
        ready, _, _ = select.select([fd], [], [], 0.2)
        if ready:
            try:
                screen += os.read(fd, 65536)
            except OSError:
                return


pump(8)
os.write(fd, b'say ok')
pump(1)
os.write(fd, b'\r')
pump(3)
registry = glob.glob(os.path.join(arm, 'cfg', 'sessions', '*.json'))
with open(os.path.join(arm, 'registry.json'), 'w') as f:
    f.write(''.join(open(p).read() for p in registry))
pump(10)
with open(os.path.join(arm, 'screen.bin'), 'wb') as f:
    f.write(screen)
os.kill(pid, signal.SIGTERM)
time.sleep(1)
try:
    os.kill(pid, signal.SIGKILL)
except ProcessLookupError:
    pass
