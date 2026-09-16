"""Response-only streaming enforcement, using real mitmproxy flow objects."""
import asyncio
import base64
import gzip
import importlib.util
from pathlib import Path
import sys
import unittest
from unittest.mock import AsyncMock, Mock
import zlib

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'proxy'))
from response_stream import PROVIDER_ENDPOINTS, ResponseStream, StreamRejected

try:
    from mitmproxy import connection, http
    HAVE_MITM = True
except ImportError:
    HAVE_MITM = False


class StreamInspectionTests(unittest.TestCase):
    def test_every_secret_split_and_single_byte_chunks(self):
        secret = b'synthetic-protected-credential'
        for split in range(1, len(secret)):
            with self.subTest(split=split):
                stream = ResponseStream([secret.decode()])
                self.assertEqual(stream.feed(b'data: ' + secret[:split]), b'data: ')
                with self.assertRaises(StreamRejected): stream.feed(secret[split:])
        stream = ResponseStream([secret.decode()])
        for byte in secret[:-1]: self.assertEqual(stream.feed(bytes([byte])), b'')
        with self.assertRaises(StreamRejected): stream.feed(secret[-1:])

    def test_safe_events_pass_immediately_and_ambiguous_suffix_flushes_at_eof(self):
        stream = ResponseStream(['synthetic-protected-credential'])
        self.assertEqual(stream.feed(b'data: hello\n\n'), b'data: hello\n\n')
        self.assertEqual(stream.feed(b'synthe'), b'')
        self.assertEqual(stream.feed(b''), b'synthe')
        self.assertEqual(stream.feed(b''), b'')
        self.assertEqual(stream.delivered, stream.decoded)

    def test_overlapping_prefixes_and_mismatch(self):
        stream = ResponseStream(['ababac', 'ababaX'])
        self.assertEqual(stream.feed(b'zzababa'), b'zz')
        self.assertEqual(stream.feed(b'baz'), b'abababaz')
        self.assertEqual(stream.feed(b''), b'')

    def test_wire_and_decoded_limits(self):
        for encoding, payload in [('identity', b'x' * 33), ('gzip', gzip.compress(b'x' * 1000))]:
            with self.subTest(encoding=encoding):
                stream = ResponseStream([], encoding, limit=32)
                with self.assertRaises(StreamRejected): stream.feed(payload)
        stream = ResponseStream([], limit=3)
        self.assertEqual(stream.feed(b'abc'), b'abc')
        with self.assertRaises(StreamRejected): stream.feed(b'd')

    def test_compressed_secret_and_truncated_or_extra_compressed_data(self):
        for encoding, compress in [('gzip', gzip.compress), ('deflate', zlib.compress)]:
            with self.subTest(encoding=encoding):
                stream = ResponseStream(['protected-secret'], encoding)
                with self.assertRaises(StreamRejected): stream.feed(compress(b'protected-secret'))
                stream = ResponseStream([], encoding)
                stream.feed(compress(b'hello')[:-2])
                with self.assertRaises(StreamRejected): stream.feed(b'')
                stream = ResponseStream([], encoding)
                with self.assertRaises(StreamRejected): stream.feed(compress(b'hello') + b'extra')

    def test_compressed_stream_one_wire_byte_at_a_time(self):
        body = b'data: snowman \xe2\x98\x83\n\n' * 10
        for encoding, compress in [('gzip', gzip.compress), ('deflate', zlib.compress)]:
            stream = ResponseStream(['unrelated-secret'], encoding)
            output = b''.join(stream.feed(bytes([byte])) for byte in compress(body))
            self.assertEqual(output + stream.feed(b''), body)


@unittest.skipUnless(HAVE_MITM, 'pinned mitmproxy dependencies required')
class ResponseStreamingTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        spec = importlib.util.spec_from_file_location('stream_test_addon', ROOT / 'proxy/addon.py')
        self.module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.module)
        self.guard = self.module.Guard()
        self.guard.control = AsyncMock(side_effect=self.control)
        self.events = []
        self.active = True
        self.fail_event = None
        self.guard.redactor.register('synthetic-protected-credential')

    async def asyncTearDown(self):
        await asyncio.gather(*self.guard.stream_cleanup)
        self.guard.done()

    async def control(self, message):
        if message['action'] == 'active': return {'active': self.active}
        if message['action'] == 'event':
            if message['event_type'] == self.fail_event: return {'recorded': False}
            self.events.append(message)
        return {'recorded': True, 'released': True}

    def flow(self, url='https://api.openai.com/v1/responses', **headers):
        flow = http.HTTPFlow(connection.Client(peername=('127.0.0.1', 1234), sockname=('127.0.0.1', 8080)),
                             connection.Server(address=('127.0.0.1', 443)), live=True)
        flow.request = http.Request.make('POST', url, b'{"stream":true}', {'Authorization':'Bearer synthetic'})
        flow.request.headers['Host'] = flow.request.host
        flow.response = http.Response.make(200, b'', {'Content-Type':'text/event-stream', 'Alt-Svc':'h3=":443"', **headers})
        flow.response.headers.pop('Content-Length', None)
        flow.metadata.update(warden_decision_id='d', warden_request_id='r')
        return flow

    async def test_authorized_stream_keeps_request_buffered_and_audits_counts(self):
        flow = self.flow()
        await self.guard.requestheaders(flow)
        await self.guard.responseheaders(flow)
        self.assertIs(flow.request.stream, False)
        self.assertTrue(callable(flow.response.stream))
        self.assertEqual(flow.response.headers['Alt-Svc'], 'clear')
        self.assertEqual(self.events[0]['event_type'], 'http.response.started')
        self.assertEqual(flow.response.stream(b'data: hello\n\n'), b'data: hello\n\n')
        self.assertEqual(flow.response.stream(b''), [])
        flow.response.trailers = http.Headers(Secret='synthetic-protected-credential')
        await self.guard.response(flow)
        self.assertIsNone(flow.response.trailers)
        self.assertNotIn('Authorization', flow.request.headers)
        self.assertFalse(self.guard.streams)
        summary = self.events[-1]['fields']['response']
        self.assertEqual(summary['stream']['phase'], 'completed')
        self.assertEqual(summary['body'], {'bytes':13, 'capture':'omitted_policy'})
        self.assertNotIn('hello', str(self.events))
        # The host accepts this event type and fields, not just our mock broker.
        from warden.server import internal
        self.assertTrue(internal(Mock(), self.events[0])['recorded'])

    async def test_non_provider_non_sse_errors_and_denied_flows_stay_buffered(self):
        for url, content_type, status in [
            ('https://example.com/v1/responses', 'text/event-stream', 200),
            ('https://api.openai.com/other', 'text/event-stream', 200),
            ('https://api.openai.com/v1/responses?other=1', 'text/event-stream', 200),
            ('https://api.openai.com/v1/responses', 'application/json', 200),
            ('https://api.openai.com/v1/responses', 'text/event-stream', 500),
        ]:
            flow = self.flow(url, **{'Content-Type': content_type})
            flow.response.status_code = status
            await self.guard.responseheaders(flow)
            self.assertIs(flow.response.stream, False)
        flow = self.flow()
        flow.metadata['warden_denied'] = True
        await self.guard.responseheaders(flow)
        self.assertIs(flow.response.stream, False)

    async def test_all_exact_provider_routes_support_sse(self):
        for host, path in PROVIDER_ENDPOINTS:
            flow = self.flow('https://' + host + path, **{'Content-Type':'Text/Event-Stream; charset=utf-8'})
            await self.guard.responseheaders(flow)
            self.assertTrue(callable(flow.response.stream))

    async def test_codex_missing_content_type_requires_explicit_stream_request(self):
        for body, expected in [(b'{"stream":true}', True), (b'{"stream":false}', False),
                               (b'{"stream":"true"}', False), (b'{}', False), (b'[]', False), (b'bad', False)]:
            flow = self.flow('https://chatgpt.com/backend-api/codex/responses')
            flow.request.content = body
            flow.response.headers.pop('Content-Type')
            await self.guard.responseheaders(flow)
            self.assertEqual(callable(flow.response.stream), expected)
        flow = self.flow()
        flow.response.headers.pop('Content-Type')
        await self.guard.responseheaders(flow)
        self.assertIs(flow.response.stream, False)

    async def test_header_echo_encoding_length_and_missing_permission_fail_before_stream(self):
        for headers in [{'X-Echo':'synthetic-protected-credential'}, {'Content-Encoding':'br'}, {'Trailer':'X-Echo'}]:
            flow = self.flow(**headers)
            await self.guard.responseheaders(flow)
            self.assertEqual(flow.response.status_code, 502)
            self.assertIs(flow.response.stream, False)
        for length in ['-1', 'no', str(16 * 1024 * 1024 + 1)]:
            flow = self.flow()
            flow.response.headers['Content-Length'] = length
            await self.guard.responseheaders(flow)
            self.assertGreaterEqual(flow.response.status_code, 400)
            self.assertIs(flow.response.stream, False)
        for decision in [None, 'd']:
            flow = self.flow()
            flow.metadata['warden_decision_id'] = decision
            self.active = False
            await self.guard.responseheaders(flow)
            self.assertEqual(flow.response.status_code, 403)

    async def test_audit_rejection_before_stream_withholds_response(self):
        self.fail_event = 'http.response.started'
        flow = self.flow()
        await self.guard.responseheaders(flow)
        self.assertEqual(flow.response.status_code, 503)
        self.assertIs(flow.response.stream, False)

    async def test_split_echo_closes_and_cleans_up_once(self):
        flow = self.flow()
        await self.guard.responseheaders(flow)
        callback = flow.response.stream
        self.assertEqual(callback(b'data: synthetic-protected-'), b'data: ')
        self.assertEqual(callback(b'credential\n\n'), [])
        self.assertIsNotNone(flow.error)
        self.assertEqual(callback(b'more'), [])
        await asyncio.gather(*self.guard.stream_cleanup)
        await self.guard.error(flow)
        self.assertEqual(sum(e['event_type'] == 'request.interrupted' for e in self.events), 1)
        self.assertNotIn('synthetic-protected-credential', str(self.events))
        self.assertFalse(self.guard.streams)
        self.assertNotIn('Authorization', flow.request.headers)

    async def test_base64_echo_and_unknown_length_limit(self):
        for payload in [base64.b64encode(b'synthetic-protected-credential'), b'x' * 17]:
            flow = self.flow()
            await self.guard.responseheaders(flow)
            if payload.startswith(b'x'): self.guard.streams[flow.id].limit = 16
            self.assertEqual(flow.response.stream(payload), [])
            self.assertIsNotNone(flow.error)
            await asyncio.gather(*self.guard.stream_cleanup)

    async def test_compression_is_decoded_and_framing_headers_removed(self):
        flow = self.flow(**{'Content-Encoding':'gzip'})
        await self.guard.responseheaders(flow)
        for header in ('Content-Encoding', 'Content-Length', 'Trailer'):
            self.assertNotIn(header, flow.response.headers)
        self.assertEqual(flow.response.stream(gzip.compress(b'data: yes\n\n')), b'data: yes\n\n')
        flow.response.stream(b'')
        await self.guard.response(flow)

    async def test_revocation_watcher_interrupts_stream_and_releases(self):
        flow = self.flow()
        await self.guard.responseheaders(flow)
        self.active = False
        task = asyncio.create_task(self.guard.watch(flow, {'decision_id':'d', 'request_id':'r', 'remaining_seconds':10}))
        self.guard.tasks[flow.id] = task
        await asyncio.wait_for(task, 2)
        await asyncio.gather(*self.guard.stream_cleanup)
        self.assertIsNotNone(flow.error)
        self.assertNotIn(flow.id, self.guard.tasks)
        self.assertEqual(self.events[-1]['event_type'], 'request.interrupted')

    async def test_final_audit_failure_kills_instead_of_replacing_stream(self):
        flow = self.flow()
        await self.guard.responseheaders(flow)
        flow.response.stream(b'data: ok\n\n')
        flow.response.stream(b'')
        self.fail_event = 'http.response'
        await self.guard.response(flow)
        self.assertIsNotNone(flow.error)
        self.assertEqual(flow.response.status_code, 200)
        self.assertNotIn('Authorization', flow.request.headers)

    async def test_empty_successful_sse_response_completes(self):
        flow = self.flow()
        await self.guard.responseheaders(flow)
        # mitmproxy skips the callback for header-only responses.
        await self.guard.response(flow)
        self.assertIsNone(flow.error)
        self.assertEqual(self.events[-1]['fields']['response']['stream']['phase'], 'completed')

    async def test_expiry_during_header_audit_never_begins_delivery(self):
        flow = self.flow()
        async def delayed_audit(message):
            result = await self.control(message)
            if message.get('event_type') == 'http.response.started': self.active = False
            return result
        self.guard.control = delayed_audit
        await self.guard.responseheaders(flow)
        self.assertIs(flow.response.stream, False)
        self.assertIsNotNone(flow.error)

    async def test_request_inspection_still_authorizes_complete_body_and_prefers_identity(self):
        import socket
        from unittest.mock import patch
        flow = self.flow()
        flow.response = None
        flow.client_conn.sni = 'api.openai.com'
        self.guard.control = AsyncMock(return_value={
            'allow':True, 'recorded':True, 'decision_id':'d', 'request_id':'r', 'remaining_seconds':60})
        with patch.object(asyncio.get_running_loop(), 'getaddrinfo', AsyncMock(return_value=[
                (socket.AF_INET, socket.SOCK_STREAM, 6, '', ('93.184.216.34', 443))])):
            await self.guard.requestheaders(flow)
            await self.guard.request(flow)
        self.assertIsNone(flow.response)
        self.assertIs(flow.request.stream, False)
        self.assertEqual(flow.request.content, b'{"stream":true}')
        self.assertEqual(flow.request.headers['Accept-Encoding'], 'identity')
        request_event = [call.args[0] for call in self.guard.control.call_args_list
                         if call.args[0].get('event_type') == 'http.request.external'][0]
        self.assertEqual(request_event['fields']['request']['body']['bytes'], len(flow.request.content))
