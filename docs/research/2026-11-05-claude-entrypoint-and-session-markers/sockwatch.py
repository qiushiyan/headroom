# Listens on a unix socket and logs each connection, to see whether a child
# handed CLAUDE_CODE_MESSAGING_SOCKET reaches for its ancestor's socket.
#
#   python3 -I sockwatch.py <socket-path> <log>
import os
import socket
import sys
import threading
import time

path, log = sys.argv[1], sys.argv[2]
try:
    os.unlink(path)
except FileNotFoundError:
    pass
server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
server.bind(path)
server.listen(16)


def handle(conn):
    conn.settimeout(3)
    try:
        data = conn.recv(4096)
    except OSError:
        data = b''
    with open(log, 'a') as f:
        f.write(f"{time.time():.0f} connect {data[:300]!r}\n")
    conn.close()


while True:
    conn, _ = server.accept()
    threading.Thread(target=handle, args=(conn,), daemon=True).start()
