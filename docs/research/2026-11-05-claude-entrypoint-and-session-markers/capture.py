# A stand-in for the Messages API on 127.0.0.1: logs each request's headers
# (credentials left out) and the parts of its body that carry a label or an
# effort, and answers /v1/messages with a one-word reply. CAP_DELAY holds each
# reply that many seconds, so a session is still running when its registry is
# read.
#
#   python3 -I capture.py <port> <log.jsonl>
import http.server
import json
import os
import re
import sys
import threading
import time

LOG = sys.argv[2]
DELAY = float(os.environ.get('CAP_DELAY', '0'))
lock = threading.Lock()


def sse(event, data):
    return f"event: {event}\ndata: {json.dumps(data)}\n\n".encode()


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *args):
        pass

    def record(self, body):
        rec = {'t': time.time(), 'method': self.command, 'path': self.path,
               'headers': {k.lower(): v for k, v in self.headers.items()
                           if k.lower() not in ('authorization', 'x-api-key')}}
        try:
            b = json.loads(body) if body else None
        except ValueError:
            b = None
        if isinstance(b, dict):
            system = b.get('system')
            if isinstance(system, str):
                system = [{'text': system}]
            rec['system_heads'] = [(x.get('text') or '')[:300] for x in system or [] if isinstance(x, dict)]
            rec['output_config'] = b.get('output_config')
            rec['effort_hits'] = sorted(set(re.findall(r'"effort"\s*:\s*"?([a-z0-9_]+)', body.decode('utf-8', 'replace'))))
        with lock, open(LOG, 'a') as f:
            f.write(json.dumps(rec) + '\n')

    def reply(self, ctype, payload):
        self.send_response(200)
        self.send_header('content-type', ctype)
        self.send_header('content-length', str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self.record(b'')
        self.send_response(404)
        self.send_header('content-length', '2')
        self.end_headers()
        self.wfile.write(b'{}')

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('content-length') or 0))
        self.record(body)
        if not self.path.startswith('/v1/messages') or 'count_tokens' in self.path:
            return self.reply('application/json', b'{"input_tokens": 10}')
        time.sleep(DELAY)
        try:
            stream = json.loads(body).get('stream')
        except ValueError:
            stream = False
        msg = {'id': 'msg_spike', 'type': 'message', 'role': 'assistant', 'model': 'stand-in',
               'content': [], 'stop_reason': None, 'stop_sequence': None,
               'usage': {'input_tokens': 10, 'output_tokens': 1,
                         'cache_creation_input_tokens': 0, 'cache_read_input_tokens': 0}}
        if not stream:
            done = dict(msg, content=[{'type': 'text', 'text': 'ok'}], stop_reason='end_turn')
            return self.reply('application/json', json.dumps(done).encode())
        self.reply('text/event-stream', b''.join([
            sse('message_start', {'type': 'message_start', 'message': msg}),
            sse('content_block_start', {'type': 'content_block_start', 'index': 0, 'content_block': {'type': 'text', 'text': ''}}),
            sse('content_block_delta', {'type': 'content_block_delta', 'index': 0, 'delta': {'type': 'text_delta', 'text': 'ok'}}),
            sse('content_block_stop', {'type': 'content_block_stop', 'index': 0}),
            sse('message_delta', {'type': 'message_delta', 'delta': {'stop_reason': 'end_turn', 'stop_sequence': None}, 'usage': {'output_tokens': 1}}),
            sse('message_stop', {'type': 'message_stop'}),
        ]))


http.server.ThreadingHTTPServer(('127.0.0.1', int(sys.argv[1])), Handler).serve_forever()
