"""Document routes use real HTTP and broker sockets with synthetic credentials."""
import base64
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import http.client
import os
import socket
import subprocess
import sys
from pathlib import Path
import tempfile
import threading
import time
import unittest

from warden.sbx import Registry, Server, handler
from warden.sbx_gateway import command
from warden.sbx_documents import DocumentAPI, document_path, MAX_BODY, MAX_RESPONSE
from test_sbx import FixtureVerifier, run_context

KEY = b'synthetic-document-signing-key-32-chars-minimum'


class DocumentAPITests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.key_file = Path(self.tmp.name) / 'key'
        self.key_file.write_bytes(KEY + b'\n')
        self.key_file.chmod(0o600)
        self.requests = []
        self.answer = {'status': 200, 'body': b'{"documents":[]}', 'headers': {'Content-Type': 'application/json'}}
        owner = self
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.dispatch()
            def do_POST(self):
                self.dispatch()
            def dispatch(self):
                body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
                owner.requests.append((self.command, self.path, dict(self.headers), body))
                if hasattr(owner, 'on_request'):
                    owner.on_request()
                self.send_response(owner.answer['status'])
                for key, value in owner.answer['headers'].items():
                    self.send_header(key, value)
                self.send_header('Content-Length', str(len(owner.answer['body'])))
                self.end_headers()
                self.wfile.write(owner.answer['body'])
            def log_message(self, *_):
                pass
        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.origin = 'http://127.0.0.1:' + str(self.server.server_port)
        self.api = DocumentAPI(self.origin, self.key_file)
        self.verifier = FixtureVerifier()
        self.registry = Registry(Path(self.tmp.name) / 'state', verifier=self.verifier, document_api=self.api)
        self.verifier.registry = self.registry
        self.value = run_context()
        self.registry.register(self.value)
        self.registry.bind_gateway(self.value, 19443)
        self.registry.configure_provider(self.value, 'synthetic-provider-fixture-secret')
        self.gateway = self.registry.gateway(self.value)
        self.begin = self.registry.begin(self.value)

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.registry.close()
        self.tmp.cleanup()

    def request(self, method='GET', path='/workspace/v1/documents', body=b'', **extra):
        return self.registry.proxy('s1', self.gateway['capability'], {'action': 'document',
            'method': method, 'path': path, 'body': base64.b64encode(body).decode(), **extra})

    def test_exact_signature_context_and_no_guest_credentials(self):
        result = self.request('POST', '/workspace/v1/documents/doc1/proposals', b'{"content":"change","baseRevision":1}')
        self.assertTrue(result['allow'])
        method, path, headers, body = self.requests[0]
        self.assertEqual(path, '/agent/v1/documents/doc1/proposals')
        encoded = headers['X-Warden-Context']
        self.assertEqual(json.loads(base64.urlsafe_b64decode(encoded + '=' * (-len(encoded) % 4))), self.value)
        canonical = '\n'.join((method, path, hashlib.sha256(body).hexdigest(),
            headers['X-Warden-Timestamp'], headers['X-Warden-Nonce'], encoded))
        self.assertEqual(headers['X-Warden-Signature'], hmac.new(KEY, canonical.encode(), hashlib.sha256).hexdigest())
        self.assertNotIn(KEY.decode(), json.dumps(result))
        self.assertNotIn(headers['X-Warden-Signature'], json.dumps(result))
        self.assertEqual(len(headers['X-Warden-Nonce']), 64)
        self.assertNotIn('Authorization', headers)
        self.assertNotIn('Cookie', headers)
        self.assertEqual(self.begin['documentBaseURL'], 'http://host.docker.internal:19443/workspace/v1/documents')
        audit = b''.join(p.read_bytes() for p in (Path(self.tmp.name) / 'state').rglob('*.jsonl'))
        self.assertNotIn(KEY, audit)
        self.assertNotIn(headers['X-Warden-Signature'].encode(), audit)
        self.assertNotIn(b'"content":"change"', audit)

    def test_allowed_routes_and_project_query(self):
        for path in ('/workspace/v1/documents?projectID=p1', '/workspace/v1/documents/doc1',
                     '/workspace/v1/documents/doc1/revisions', '/workspace/v1/documents/doc1/proposals',
                     '/workspace/v1/documents?projectID=p1&offset=100', '/workspace/v1/documents/doc1/revisions?offset=8'):
            self.assertTrue(self.request(path=path)['allow'])
            self.assertEqual(self.requests[-1][1], path.replace('/workspace/v1/', '/agent/v1/'))

    def test_path_method_body_and_context_forgery_denied_before_upstream(self):
        for path in ('/workspace/v1/documents?projectID=p2', '/workspace/v1/documents?url=http://evil',
                     '/workspace/v1/documents/../auth', '/workspace/v1/documents/%2e%2e',
                     '/workspace/v1/documents/doc1/proposals/accept', '/workspace/v1/documents/doc1#fragment',
                     '/workspace/v1/documents/', '/workspace/v1/documents/doc1?projectID=p1',
                     '/workspace/v1/documents?offset=-1', '/workspace/v1/documents?offset=1&offset=2',
                     '/workspace/v1/documents?offset=1&target=evil', '/workspace/v1/documents/doc1?offset=0'):
            self.assertFalse(self.request(path=path)['allow'], path)
        self.assertFalse(self.request(method='DELETE')['allow'])
        self.assertFalse(self.request(body=b'{}')['allow'])
        self.assertFalse(self.request('POST', '/workspace/v1/documents/doc1/proposals', b'[]')['allow'])
        self.assertFalse(self.request('POST', '/workspace/v1/documents/doc1/proposals', b'x' * (MAX_BODY + 1))['allow'])
        self.assertFalse(self.request(context={**self.value, 'projectID': 'p2'})['allow'])
        self.assertEqual(self.requests, [])

    def test_saved_comment_replies_allowed_but_editor_drafts_denied(self):
        path = '/workspace/v1/documents/doc1/comments/replies?offset=8'
        self.assertTrue(self.request(path=path)['allow'])
        self.assertEqual(self.requests[-1][1], path.replace('/workspace/v1/', '/agent/v1/'))
        path = '/workspace/v1/documents/doc1/comments/thread1/replies'
        body = b'{"id":"reply1","baseRevision":2,"text":"Explanation"}'
        self.assertTrue(self.request('POST', path, body)['allow'])
        method, upstream, headers, received = self.requests[-1]
        self.assertEqual(received, body)
        canonical = '\n'.join((method, upstream, hashlib.sha256(body).hexdigest(),
            headers['X-Warden-Timestamp'], headers['X-Warden-Nonce'], headers['X-Warden-Context']))
        self.assertEqual(headers['X-Warden-Signature'], hmac.new(KEY, canonical.encode(), hashlib.sha256).hexdigest())
        count = len(self.requests)
        for suffix in ('collaboration', 'snapshot', 'comments', 'comments/thread1',
                       'comments/%2e%2e/replies', 'comments/thread1/replies?offset=1'):
            for method in ('GET', 'POST'):
                self.assertFalse(self.request(method, '/workspace/v1/documents/doc1/' + suffix,
                    b'{}' if method == 'POST' else b'')['allow'])
        self.assertEqual(len(self.requests), count)

    def test_gateway_cannot_use_other_sandbox_capability(self):
        other = run_context(sandboxID='s2', runtimeName='sbx-two')
        self.registry.register(other)
        with self.assertRaises(ValueError):
            self.registry.proxy('s2', self.gateway['capability'], {'action': 'document'})
        self.assertEqual(self.requests, [])

    def test_missing_config_inactive_or_failed_proof_fail_closed(self):
        self.registry.document_api = None
        self.assertFalse(self.request()['allow'])
        self.assertNotIn('documentBaseURL', self.registry.begin(self.value, renew=True))
        self.registry.document_api = self.api
        self.verifier.enabled = False
        self.assertFalse(self.request()['allow'])
        self.verifier.enabled = True
        self.assertFalse(self.request()['allow'])
        self.assertEqual(self.requests, [])

    def test_end_during_upstream_response_does_not_deadlock_or_deliver(self):
        self.on_request = lambda: self.registry.end(self.value)
        self.assertFalse(self.request()['allow'])
        self.assertEqual(len(self.requests), 1)

    def test_lease_expiring_during_final_proof_blocks_delivery(self):
        expiry = self.registry.bindings['s1']['lease']['expires_at']
        verify = self.verifier.verify
        def delay_final_proof():
            self.registry.clock = lambda: expiry - 1
            def advancing_verify(binding, phase):
                proof = verify(binding, phase)
                self.registry.clock = lambda: expiry + 1
                return proof
            self.verifier.verify = advancing_verify
        self.on_request = delay_final_proof
        self.assertFalse(self.request()['allow'])
        self.assertEqual(len(self.requests), 1)

    def test_error_status_preserved_but_redirect_never_followed(self):
        self.answer.update(status=409, body=b'{"error":"revision_conflict"}')
        self.assertEqual(self.request()['status'], 409)
        self.answer.update(status=302, headers={'Location': self.origin + '/auth', 'Content-Type': 'application/json'})
        self.assertFalse(self.request()['allow'])
        self.assertEqual(len(self.requests), 2)
        self.assertFalse(any(item[1] == '/auth' for item in self.requests))

    def test_non_json_compressed_oversize_and_auth_echo_denied(self):
        for body, headers in ((b'plain', {'Content-Type': 'text/plain'}),
                              (b'{}', {'Content-Type': 'application/json', 'Content-Encoding': 'gzip'}),
                              (b' ' * (MAX_RESPONSE + 1), {'Content-Type': 'application/json'}),
                              (json.dumps({'echo': KEY.decode()}).encode(), {'Content-Type': 'application/json'})):
            self.answer.update(body=body, headers=headers)
            self.assertFalse(self.request()['allow'])
        self.answer.update(headers={'Content-Type': 'application/json'})
        def echo():
            self.answer['body'] = json.dumps({'echo': self.requests[-1][2]['X-Warden-Signature']}).encode()
        self.on_request = echo
        self.assertFalse(self.request()['allow'])

    @unittest.skipUnless((Path(sys.executable).parent / 'mitmdump').is_file(), 'pinned mitmdump required')
    def test_real_gateway_to_broker_to_document_http_and_revocation(self):
        # Independent ports and a fresh binding keep this test isolated from all
        # existing local apps, sandboxes and Warden processes.
        with socket.socket() as allocation:
            allocation.bind(('127.0.0.1', 0))
            port = allocation.getsockname()[1]
        self.registry.end(self.value)
        new = run_context(generation='2', runID='r2')
        self.registry.register(new)
        binding = self.registry.bindings['s1']
        binding['gateway_port'] = None
        self.registry.bind_gateway(new, port)
        self.registry.begin(new)
        path = Path(self.tmp.name) / 'control.sock'
        broker = Server(str(path), handler(self.registry))
        path.chmod(0o600)
        threading.Thread(target=broker.serve_forever, daemon=True).start()
        config = Path(self.tmp.name) / 'gateway.json'
        config.write_text(json.dumps({**self.registry.gateway(new), 'socketPath': str(path)}))
        config.chmod(0o600)
        log = (Path(self.tmp.name) / 'gateway.log').open('w+')
        process = subprocess.Popen(command(str(Path(sys.executable).parent / 'mitmdump'), Path(self.tmp.name), port),
            env={**os.environ, 'WARDEN_SBX_GATEWAY_CONFIG': str(config)}, stdout=log, stderr=log)
        try:
            deadline = time.monotonic() + 10
            while True:
                try:
                    with socket.create_connection(('127.0.0.1', port), timeout=0.2):
                        break
                except OSError:
                    if process.poll() is not None or time.monotonic() >= deadline:
                        self.fail('document test gateway did not start')
                    time.sleep(0.05)
            client = http.client.HTTPConnection('127.0.0.1', port, timeout=7)
            headers = {'Host': 'host.docker.internal:' + str(port), 'Cookie': 'owner=forged',
                       'Authorization': 'Bearer forged', 'X-Warden-Context': 'forged'}
            client.request('GET', '/workspace/v1/documents', headers=headers)
            response = client.getresponse()
            self.assertEqual((response.status, response.read()), (200, b'{"documents":[]}'))
            self.assertEqual(len(self.requests), 1)
            self.assertNotIn('Authorization', self.requests[0][2])
            self.assertNotIn('Cookie', self.requests[0][2])
            self.assertNotEqual(self.requests[0][2]['X-Warden-Context'], 'forged')
            self.registry.end(new)
            client.request('GET', '/workspace/v1/documents', headers=headers)
            response = client.getresponse()
            self.assertEqual(response.status, 503)
            response.read()
            client.close()
            self.assertEqual(len(self.requests), 1)
            log.flush(); log.seek(0)
            self.assertNotIn(KEY.decode(), log.read())
        finally:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill(); process.wait(timeout=5)
            log.close()
            broker.shutdown(); broker.server_close()

    def test_origin_and_key_validation(self):
        for origin in ('https://127.0.0.1:8080', 'http://evil.test:8080', 'http://127.1:8080',
                       'http://127.0.0.1:8080/', 'http://127.0.0.1', 'http://user@localhost:8080',
                       'http://localhost:8080?target=evil', 'http://localhost:8080#fragment'):
            with self.assertRaises(ValueError, msg=origin):
                DocumentAPI(origin, self.key_file)
        self.key_file.chmod(0o644)
        with self.assertRaises(ValueError): DocumentAPI(self.origin, self.key_file)
        self.key_file.chmod(0o600)
        link = Path(self.tmp.name) / 'link'
        link.symlink_to(self.key_file)
        with self.assertRaises(OSError): DocumentAPI(self.origin, link)
        for key in (b'short', b' ' * 64, b'nonascii-' + bytes([255]) * 64):
            self.key_file.write_bytes(key)
            with self.assertRaises(ValueError): DocumentAPI(self.origin, self.key_file)


if __name__ == '__main__':
    unittest.main()
