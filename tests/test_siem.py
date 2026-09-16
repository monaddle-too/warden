import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
from pathlib import Path
import tempfile
import time
import threading
import unittest
from unittest.mock import patch

from warden.siem import DeliveryError, Shipper, configuration, post, read_token
from test_ocsf import native


class SIEMTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.state = Path(self.tmp.name)
        (self.state / 'siem.json').write_text(json.dumps({'endpoint': 'https://siem.example/api/v1/events'}))
        (self.state / 'siem-token').write_text('a' * 64)
        (self.state / 'siem-token').chmod(0o600)
        (self.state / 'audit').mkdir()
        self.audit = self.state / 'audit/events.jsonl'
        self.audit.touch()
        self.calls = []
        self.shipper = Shipper(self.state, self.accept)

    def tearDown(self):
        self.shipper.close()
        self.tmp.cleanup()

    def accept(self, endpoint, source, token, key, body):
        self.calls.append((endpoint, source, token, key, body))
        return {'id': 'a' * 32, 'state': 'queued', 'accepted': len(body.splitlines()), 'rejected': 0, 'problems': []}

    def append(self, event):
        with self.audit.open('ab') as stream: stream.write((json.dumps(event) + '\n').encode())

    def restart(self):
        self.shipper.close()
        self.shipper = Shipper(self.state, self.accept)

    def test_lost_receipt_restart_replays_exact_bytes_before_new_events(self):
        self.append(native(request_id='r', request={'method': 'GET', 'host': 'example.com', 'path': '/'}))
        def lost(*args):
            self.accept(*args)
            raise TimeoutError('possibly accepted')
        self.shipper.transport = lost
        with self.assertRaises(TimeoutError): self.shipper.step()
        self.assertEqual(self.shipper.load()[0]['offset'], 0)
        self.append(native('http.response', request_id='r', status=200))
        self.restart()
        self.assertTrue(self.shipper.step())
        self.assertEqual(self.calls[0], self.calls[1])
        self.assertTrue(self.shipper.step())
        response = json.loads(self.calls[2][-1])
        self.assertEqual(response['dst_endpoint']['hostname'], 'example.com')
        self.assertFalse(self.shipper.step())
        self.assertEqual(self.shipper.status()['delivered_events'], 2)
        self.restart()
        self.assertFalse(self.shipper.step())

    def test_partial_line_and_record_limit(self):
        self.append(native())
        with self.audit.open('ab') as stream: stream.write(json.dumps(native('dns.query')).encode())
        self.assertTrue(self.shipper.step())
        self.assertFalse(self.shipper.step())
        with self.audit.open('ab') as stream: stream.write(b'\n')
        self.append(native())
        with patch('warden.siem.MAX_RECORDS', 1):
            self.assertTrue(self.shipper.step())
            self.assertTrue(self.shipper.step())
        self.assertEqual(len(self.calls), 3)

    def test_crash_after_receipt_before_cursor_commit_replays_pending(self):
        self.append(native())
        data, _ = self.shipper.load()
        self.shipper.prepare(data)
        with patch.object(self.shipper, 'save', side_effect=OSError('disk unavailable')):
            with self.assertRaises(OSError): self.shipper.step()
        self.restart()
        self.assertTrue(self.shipper.step())
        self.assertEqual(self.calls[0], self.calls[1])
        self.assertEqual(self.shipper.status()['delivered_events'], 1)

    def test_http_failure_retains_pending_and_token_rotation_keeps_batch(self):
        self.append(native())
        def unavailable(*args):
            self.accept(*args)
            raise DeliveryError('SIEM HTTP 503; pending batch retained')
        self.shipper.transport = unavailable
        with self.assertRaises(DeliveryError): self.shipper.step()
        (self.state / 'siem-token').write_text('b' * 64)
        self.shipper.transport = self.accept
        self.assertTrue(self.shipper.step())
        self.assertEqual(self.calls[0][3:], self.calls[1][3:])
        self.assertEqual(self.calls[1][2], 'b' * 64)

    def test_connection_outage_restart_and_periodic_recovery_drains_backlog(self):
        received = []
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args): pass
            def do_POST(self):
                body = self.rfile.read(int(self.headers['Content-Length']))
                received.append((self.headers['Idempotency-Key'], body))
                receipt = json.dumps({'id': 'a' * 32, 'state': 'queued',
                                      'accepted': len(body.splitlines()), 'rejected': 0}).encode()
                self.send_response(202)
                self.send_header('Content-Length', str(len(receipt)))
                self.end_headers()
                self.wfile.write(receipt)
        # Reserve a real loopback port without listening: initial TCP connects fail.
        server = HTTPServer(('127.0.0.1', 0), Handler, bind_and_activate=False)
        server.server_bind()
        worker = None
        def connection(*args, **kwargs):
            return http.client.HTTPConnection('127.0.0.1', server.server_address[1], timeout=2)
        try:
            with patch('warden.siem.http.client.HTTPSConnection', side_effect=connection), patch('warden.siem.time.time', return_value=1000) as clock, patch('warden.siem.random.random', return_value=0), patch('warden.siem.MAX_RECORDS', 1):
                self.shipper.transport = post
                self.append(native(event_id='first'))
                self.assertFalse(self.shipper.poll())
                status = self.shipper.status()
                self.assertEqual(status['next_retry_at'], 1002)
                self.assertEqual(status['consecutive_failures'], 1)
                self.assertEqual(status['pending_events'], 1)
                self.assertGreater(status['pending_bytes'], 0)
                data, first_body = self.shipper.load()
                first_key = data['pending']['key']
                self.append(native(event_id='second'))
                self.restart()
                self.shipper.transport = post
                self.assertFalse(self.shipper.poll())
                self.assertEqual(self.shipper.status()['consecutive_failures'], 1)
                clock.return_value = 1002
                self.assertFalse(self.shipper.poll())
                self.assertEqual(self.shipper.status()['next_retry_at'], 1006)
                self.assertEqual(self.shipper.load()[1], first_body)
                self.assertEqual(self.shipper.load()[0]['offset'], 0)
                server.server_activate()
                worker = threading.Thread(target=server.serve_forever, daemon=True)
                worker.start()
                self.append(native(event_id='third'))
                clock.return_value = 1006
                self.assertTrue(self.shipper.poll())
                self.assertEqual(received[0], (first_key, first_body))
                self.assertTrue(self.shipper.poll())
                self.assertTrue(self.shipper.poll())
                self.assertFalse(self.shipper.poll())
                self.assertEqual([json.loads(body)['metadata']['uid'] for _, body in received], ['first', 'second', 'third'])
                self.assertEqual(self.shipper.status()['unshipped_bytes'], 0)
                self.assertEqual(self.shipper.status()['consecutive_failures'], 0)
                self.assertIsNone(self.shipper.status()['next_retry_at'])
                self.assertIsNone(self.shipper.status()['error'])
        finally:
            if worker: server.shutdown(); worker.join()
            server.server_close()

    def test_periodic_retry_is_capped_and_does_not_expose_exception_secrets(self):
        self.append(native())
        def unavailable(*args): raise TimeoutError('PRIVATE credential or payload')
        self.shipper.transport = unavailable
        with patch('warden.siem.time.time', return_value=1000) as clock, patch('warden.siem.random.random', return_value=0.5):
            for attempt in range(10):
                self.assertFalse(self.shipper.poll())
                status = self.shipper.status()
                self.assertLessEqual(status['next_retry_at'] - clock.return_value, 60)
                self.assertNotIn('PRIVATE', json.dumps(status))
                clock.return_value = status['next_retry_at']
            self.assertEqual(status['next_retry_at'] - status['last_attempt_at'], 60)

    def test_byte_limit_and_body_retention(self):
        self.append(native('http.response', response={'body': {'bytes': 5, 'content': 'PRIVATE', 'capture': 'complete_redacted'}}))
        self.append(native())
        with patch('warden.siem.MAX_BODY', 1500):
            self.assertTrue(self.shipper.step())
            self.assertTrue(self.shipper.step())
        self.assertNotIn(b'PRIVATE', self.calls[0][-1])
        self.assertEqual(self.calls[0][3], 'warden-' + hashlib.sha256(self.calls[0][4]).hexdigest())

    def test_rejection_and_wrong_receipts_never_advance(self):
        self.append(native())
        for fields in ({'accepted': 0, 'rejected': 1}, {'accepted': 2}, {'accepted': True}, {'state': 'dead_letter'}, {'id': 'invalid'}, {'problems': [{'reason': 'bad'}]}):
            def reject(*args): return {**self.accept(*args), **fields}
            self.shipper.transport = reject
            with self.assertRaises(DeliveryError): self.shipper.step()
            data, body = self.shipper.load()
            self.assertEqual(data['offset'], 0)
            self.assertIsNotNone(body)
        self.assertTrue(all(call == self.calls[0] for call in self.calls))

    def test_corrupt_or_oversized_record_does_not_discard_valid_prefix(self):
        self.append(native())
        with self.audit.open('ab') as stream: stream.write(b'not-json\n')
        self.assertTrue(self.shipper.step())
        offset = self.shipper.load()[0]['offset']
        with self.assertRaises(DeliveryError): self.shipper.step()
        self.assertEqual(self.shipper.load()[0]['offset'], offset)

    def test_oversized_projection_is_not_sent(self):
        self.append(native(reason='x' * (256 * 1024)))
        with self.assertRaises(DeliveryError): self.shipper.step()
        self.assertEqual(self.calls, [])
        self.assertEqual(self.shipper.load()[0]['offset'], 0)

    def test_rotation_truncation_and_rewrite_stop_shipping(self):
        self.append(native())
        self.shipper.step()
        original = self.audit.read_bytes()
        self.audit.write_bytes(original.replace(b'a' * 64, b'b' * 64))
        with self.assertRaises(DeliveryError): self.shipper.step()
        self.audit.write_bytes(b'')
        with self.assertRaises(DeliveryError): self.shipper.step()
        self.audit.rename(self.audit.with_suffix('.old'))
        self.audit.write_bytes(original)
        with self.assertRaises(DeliveryError): self.shipper.step()

    def test_long_outage_still_retries_exact_pending_batch(self):
        self.append(native())
        data, _ = self.shipper.load()
        body = self.shipper.prepare(data)
        data['pending']['created'] = time.time() - 30 * 86400
        self.shipper.save(data, body)
        self.assertTrue(self.shipper.poll())
        self.assertEqual(self.calls[0][3:], (data['pending']['key'], body))
        self.assertEqual(self.shipper.status()['unshipped_bytes'], 0)

    def test_destination_and_token_guards(self):
        for endpoint in ('http://siem.example/api/v1/events', 'https://user:pass@siem.example/api/v1/events', 'https://siem.example/api/v1/events?token=secret'):
            (self.state / 'siem.json').write_text(json.dumps({'endpoint': endpoint}))
            with self.assertRaises(DeliveryError): configuration(self.state)
        (self.state / 'siem.json').write_text(json.dumps({'endpoint': 'https://other.example/api/v1/events'}))
        with self.assertRaises(DeliveryError): Shipper(self.state)
        (self.state / 'siem-token').chmod(0o644)
        with self.assertRaises(DeliveryError): read_token(self.state / 'siem-token')

    def test_http_headers_and_no_redirects(self):
        with patch('warden.siem.http.client.HTTPSConnection') as factory:
            response = factory.return_value.getresponse.return_value
            response.status = 202
            response.read.return_value = b'{"accepted":1}'
            self.assertEqual(post('https://siem.example/api/v1/events', 'warden', 'secret', 'key', b'{}\n'), {'accepted': 1})
            args, kwargs = factory.return_value.request.call_args
            self.assertEqual(args, ('POST', '/api/v1/events'))
            self.assertEqual(kwargs['headers']['Authorization'], 'Bearer secret')
            self.assertEqual(kwargs['headers']['Content-Type'], 'application/x-ndjson')
            self.assertEqual(kwargs['headers']['X-OCSF-Source'], 'warden')
            self.assertEqual(kwargs['headers']['Idempotency-Key'], 'key')
            response.status = 302
            with self.assertRaises(DeliveryError): post('https://siem.example/api/v1/events', 'warden', 'secret', 'key', b'{}\n')
            self.assertEqual(factory.return_value.request.call_count, 2)


if __name__ == '__main__': unittest.main()
