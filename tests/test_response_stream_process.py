"""Loopback upstream + real pinned mitmdump; no provider account or credentials.

The fixture supplies authorization/routing; production response hooks, stream
inspection, watcher, transport cancellation and audit lifecycle run unchanged.
"""
import contextlib
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import zlib

ROOT = Path(__file__).resolve().parents[1]
MITMDUMP = Path(sys.executable).parent / 'mitmdump'
FIRST = b'data: first\n\n'
SECOND = b'data: second\n\n'

FIXTURE = '''
import asyncio, json, os, sys
from pathlib import Path
sys.path.insert(0, os.environ['STREAM_PROXY_SOURCE'])
from addon import Guard

class Fixture(Guard):
    async def running(self): pass
    async def control(self, message):
        root = Path(os.environ['STREAM_FIXTURE_STATE'])
        if message['action'] == 'active':
            return {'active': not (root / 'revoked').exists()}
        if message['action'] == 'event' and (root / ('fail-' + message['event_type'])).exists():
            return {'recorded': False}
        with (root / 'events').open('a') as out:
            out.write(json.dumps(message) + '\\n')
        return {'recorded': True, 'released': True}
    async def request(self, flow):
        flow.metadata.update(warden_decision_id=flow.id, warden_request_id=flow.id)
        flow.request.scheme = 'http'
        flow.request.host = '127.0.0.1'
        flow.request.port = int(os.environ['STREAM_UPSTREAM_PORT'])
        flow.server_conn.address = ('127.0.0.1', flow.request.port)
        self.redactor.register('synthetic-protected-credential')
        self.tasks[flow.id] = asyncio.create_task(self.watch(flow, {
            'remaining_seconds':60, 'decision_id':flow.id, 'request_id':flow.id}))
    async def responseheaders(self, flow):
        # Canonical production route, after loopback fixture dispatch.
        flow.request.scheme = 'https'
        flow.request.host = 'api.openai.com'
        flow.request.port = 443
        flow.request.path = '/v1/responses'
        if (Path(os.environ['STREAM_FIXTURE_STATE']) / 'missing-type').exists():
            flow.request.host = 'chatgpt.com'
            flow.request.path = '/backend-api/codex/responses'
        await super().responseheaders(flow)
        state = self.streams.get(flow.id)
        if state and (Path(os.environ['STREAM_FIXTURE_STATE']) / 'small-limit').exists():
            state.limit = 16

addons = [Fixture()]
'''


@unittest.skipUnless(MITMDUMP.is_file(), 'pinned mitmdump executable required')
class StreamingProcessTests(unittest.TestCase):
    @contextlib.contextmanager
    def gateway(self, *, encoding='identity', first=FIRST, second=SECOND, flags=(), trailers=None):
        release = threading.Event()
        received = threading.Event()
        upstream_finished = threading.Event()

        class Upstream(BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'
            def log_message(self, *_): pass
            def handle(self):
                try: super().handle()
                except ConnectionResetError: pass  # Expected when enforcement closes a stream.
            def do_POST(self):
                self.rfile.read(int(self.headers.get('Content-Length', '0')))
                received.set()
                self.send_response(200)
                if 'missing-type' not in flags:
                    self.send_header('Content-Type', 'text/event-stream')
                self.send_header('Transfer-Encoding', 'chunked')
                self.send_header('Alt-Svc', 'h3=":443"')
                if encoding != 'identity': self.send_header('Content-Encoding', encoding)
                if trailers: self.send_header('Trailer', 'X-Echo')
                self.end_headers()
                compressor = zlib.compressobj(wbits=31 if encoding == 'gzip' else 15) if encoding != 'identity' else None
                def send(data):
                    if data:
                        self.wfile.write(('%x\r\n' % len(data)).encode() + data + b'\r\n')
                        self.wfile.flush()
                try:
                    send(compressor.compress(first) + compressor.flush(zlib.Z_SYNC_FLUSH) if compressor else first)
                    if not release.wait(15): return
                    send(compressor.compress(second) + compressor.flush(zlib.Z_FINISH) if compressor else second)
                    self.wfile.write(b'0\r\n' + (b'X-Echo: ' + trailers + b'\r\n' if trailers else b'') + b'\r\n')
                    self.wfile.flush()
                except (OSError, ValueError): pass
                finally: upstream_finished.set()

        with tempfile.TemporaryDirectory(prefix='warden-stream-') as temp:
            state = Path(temp)
            for flag in flags: (state / flag).touch()
            script = state / 'fixture.py'
            script.write_text(FIXTURE)
            upstream = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
            thread = threading.Thread(target=upstream.serve_forever, daemon=True)
            thread.start()
            with socket.socket() as allocator:
                allocator.bind(('127.0.0.1', 0))
                port = allocator.getsockname()[1]
            with (state / 'proxy.log').open('w+') as log:
                process = subprocess.Popen([str(MITMDUMP), '--quiet', '--listen-host', '127.0.0.1',
                    '--listen-port', str(port), '--set', 'connection_strategy=lazy',
                    '--set', 'upstream_cert=false',
                    '--set', 'confdir=' + str(state / 'ca'), '-s', str(script)],
                    env={**os.environ, 'STREAM_PROXY_SOURCE':str(ROOT / 'proxy'),
                         'STREAM_FIXTURE_STATE':str(state), 'STREAM_UPSTREAM_PORT':str(upstream.server_port)},
                    stdout=log, stderr=log)
                client = http.client.HTTPConnection('127.0.0.1', port, timeout=3)
                try:
                    deadline = time.monotonic() + 10
                    while True:
                        try:
                            with socket.create_connection(('127.0.0.1', port), timeout=.1): break
                        except OSError:
                            if process.poll() is not None or time.monotonic() > deadline:
                                log.seek(0)
                                self.fail('proxy startup failed: ' + log.read())
                            time.sleep(.025)
                    yield client, state, release, received, upstream_finished
                finally:
                    client.close()
                    release.set()
                    process.terminate()
                    try: process.wait(timeout=5)
                    except subprocess.TimeoutExpired: process.kill(); process.wait(timeout=5)
                    upstream.shutdown(); upstream.server_close()
                    log.seek(0)
                    output = log.read()
                    self.assertNotIn('Traceback', output, output)
                    self.assertNotIn('mitmproxy has crashed', output, output)

    def start(self, client):
        client.request('POST', 'http://api.openai.com/v1/responses', b'{"stream":true}', {'Content-Type':'application/json'})
        return client.getresponse()

    def events(self, state):
        path = state / 'events'
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def wait_event(self, state, *, action='event', event_type=None):
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            for event in self.events(state):
                if event['action'] == action and (not event_type or event.get('event_type') == event_type): return event
            time.sleep(.025)
        self.fail('event missing: ' + str((action, event_type, self.events(state))))

    def test_first_event_delivered_before_upstream_completion_identity_gzip_deflate(self):
        for encoding in ('identity', 'gzip', 'deflate'):
            with self.subTest(encoding=encoding), self.gateway(encoding=encoding) as (client, state, release, received, finished):
                response = self.start(client)
                self.assertEqual(response.status, 200)
                self.assertTrue(received.is_set())
                self.assertEqual(response.read(len(FIRST)), FIRST)
                self.assertFalse(finished.is_set())
                self.assertEqual(response.getheader('Alt-Svc'), 'clear')
                self.assertIsNone(response.getheader('Content-Encoding'))
                release.set()
                self.assertEqual(response.read(), SECOND)
                event = self.wait_event(state, event_type='http.response')
                self.assertEqual(event['fields']['response']['stream']['forwarded_bytes'], len(FIRST + SECOND))
                self.wait_event(state, action='egress.finish')

    def test_split_secret_terminates_transport_without_leaking_prefix(self):
        with self.gateway(first=b'data: synthetic-protected-', second=b'credential\n\n') as (client, state, release, _, _):
            response = self.start(client)
            self.assertEqual(response.read(6), b'data: ')
            release.set()
            with self.assertRaises(http.client.IncompleteRead) as error: response.read()
            self.assertNotIn(b'synthetic', error.exception.partial)
            event = self.wait_event(state, event_type='request.interrupted')
            self.assertIn('protected credential', event['fields']['reason'])
            self.wait_event(state, action='egress.finish')

    def test_codex_missing_type_streams_before_eof(self):
        with self.gateway(flags=('missing-type',)) as (client, state, release, _, finished):
            response = self.start(client)
            self.assertEqual(response.read(len(FIRST)), FIRST)
            self.assertFalse(finished.is_set())
            release.set()
            self.assertEqual(response.read(), SECOND)
            self.wait_event(state, event_type='http.response.started')

    def test_revocation_closes_idle_stream_without_waiting_for_upstream_eof(self):
        with self.gateway() as (client, state, release, _, finished):
            response = self.start(client)
            self.assertEqual(response.read(len(FIRST)), FIRST)
            (state / 'revoked').touch()
            with self.assertRaises(http.client.IncompleteRead): response.read()
            self.assertFalse(finished.is_set())
            self.assertFalse(release.is_set())
            self.wait_event(state, event_type='request.interrupted')
            self.wait_event(state, action='egress.finish')

    def test_client_cancellation_releases_decision(self):
        with self.gateway() as (client, state, _, _, _):
            response = self.start(client)
            self.assertEqual(response.read(len(FIRST)), FIRST)
            response.close(); client.close()
            self.wait_event(state, event_type='request.interrupted')
            self.wait_event(state, action='egress.finish')

    def test_audit_failure_before_first_byte_withholds_body(self):
        with self.gateway(flags=['fail-http.response.started']) as (client, state, _, _, _):
            with self.assertRaises(http.client.RemoteDisconnected): self.start(client)
            self.wait_event(state, action='egress.finish')

    def test_final_audit_failure_closes_incomplete_stream(self):
        with self.gateway(flags=['fail-http.response']) as (client, state, release, _, _):
            response = self.start(client)
            self.assertEqual(response.read(len(FIRST)), FIRST)
            release.set()
            with self.assertRaises(http.client.IncompleteRead): response.read()
            self.wait_event(state, action='egress.finish')

    def test_unknown_length_limit_interrupts(self):
        with self.gateway(flags=['small-limit']) as (client, state, release, _, _):
            response = self.start(client)
            self.assertEqual(response.read(len(FIRST)), FIRST)
            release.set()
            with self.assertRaises(http.client.IncompleteRead): response.read()
            event = self.wait_event(state, event_type='request.interrupted')
            self.assertIn('inspection limit', event['fields']['reason'])

    def test_declared_trailers_rejected_before_streaming(self):
        with self.gateway(trailers=b'synthetic-protected-credential') as (client, state, release, _, _):
            with self.assertRaises(http.client.RemoteDisconnected): self.start(client)
            self.wait_event(state, action='egress.finish')

    def test_http2_over_intercepted_tls_streams_and_revokes(self):
        import h2.connection
        import h2.events
        with self.gateway() as (client, state, _, _, finished):
            client.connect()
            raw = client.sock
            raw.sendall(b'CONNECT api.openai.com:443 HTTP/1.1\r\nHost: api.openai.com:443\r\n\r\n')
            headers = b''
            while not headers.endswith(b'\r\n\r\n'): headers += raw.recv(1)
            self.assertIn(b'200', headers)
            context = ssl.create_default_context(cafile=str(state / 'ca/mitmproxy-ca-cert.pem'))
            context.set_alpn_protocols(['h2'])
            with context.wrap_socket(raw, server_hostname='api.openai.com') as tls:
                client.sock = None
                self.assertEqual(tls.selected_alpn_protocol(), 'h2')
                connection = h2.connection.H2Connection()
                connection.initiate_connection()
                connection.send_headers(1, [(':method','POST'), (':scheme','https'),
                    (':authority','api.openai.com'), (':path','/v1/responses'), ('content-length','2')])
                connection.send_data(1, b'{}', end_stream=True)
                tls.sendall(connection.data_to_send())
                body = b''
                while len(body) < len(FIRST):
                    wire = tls.recv(65536)
                    self.assertTrue(wire)
                    for event in connection.receive_data(wire):
                        if isinstance(event, h2.events.DataReceived):
                            body += event.data
                            connection.acknowledge_received_data(event.flow_controlled_length, event.stream_id)
                        self.assertNotIsInstance(event, h2.events.StreamEnded)
                    if outgoing := connection.data_to_send(): tls.sendall(outgoing)
                self.assertEqual(body, FIRST)
                self.assertFalse(finished.is_set())
                (state / 'revoked').touch()
                while wire := tls.recv(65536):
                    for event in connection.receive_data(wire):
                        self.assertNotIsInstance(event, h2.events.DataReceived)
                self.wait_event(state, event_type='request.interrupted')
                self.wait_event(state, action='egress.finish')
