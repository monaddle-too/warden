"""Real mitmproxy flow objects and private broker transport, synthetic secrets."""
import asyncio
import importlib.util
from pathlib import Path
import socket
import sys
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import AsyncMock, Mock, patch

from warden.sbx import Registry, Server, handler
from test_sbx import FixtureVerifier, run_context

try:
    from mitmproxy import http, connection
    HAVE_MITM = True
except ImportError:
    HAVE_MITM = False


@unittest.skipUnless(HAVE_MITM, 'pinned mitmproxy dependencies required')
class SbxProxyTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'proxy'))
        import sbx_addon
        self.module = sbx_addon
        self.tmp = tempfile.TemporaryDirectory()
        self.verifier = FixtureVerifier()
        self.registry = Registry(self.tmp.name, verifier=self.verifier)
        self.verifier.registry = self.registry
        self.value = run_context()
        self.registry.register(self.value)
        self.registry.bind_gateway(self.value, 19443)
        self.registry.configure_provider(self.value, 'synthetic-provider-fixture-secret')
        self.registry.begin(self.value)
        self.path = Path(self.tmp.name) / 'control.sock'
        self.server = Server(str(self.path), handler(self.registry))
        self.path.chmod(0o600)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        binding = self.registry.gateway(self.value)
        self.guard = sbx_addon.SbxGuard(str(self.path), binding['bindingID'], binding['capability'], 19443)
        self.lookup = patch.object(asyncio.get_running_loop(), 'getaddrinfo', new=AsyncMock(return_value=[
            (socket.AF_INET, socket.SOCK_STREAM, 6, '', ('93.184.216.34', 443))]))
        self.resolver = self.lookup.start()

    async def asyncTearDown(self):
        self.guard.done()
        self.lookup.stop()
        await asyncio.to_thread(self.server.shutdown)
        self.server.server_close()
        self.registry.close()
        self.tmp.cleanup()

    def flow(self, url='http://host.docker.internal:19443/openai/v1/responses', method='POST', headers=None, sni=None):
        client = connection.Client(peername=('127.0.0.1', 23456), sockname=('127.0.0.1', 19443), sni=sni)
        server = connection.Server(address=('127.0.0.1', 19443))
        flow = http.HTTPFlow(client, server, live=True)
        flow.request = http.Request.make(method, url, b'{"input":"synthetic"}', {
            'Content-Type': 'application/json', 'Authorization': 'Bearer hostile-placeholder', **(headers or {})})
        return flow

    async def test_reverse_provider_route_injects_only_after_scoped_authorization(self):
        flow = self.flow(headers={'Cookie': 'secret=cookie', 'OpenAI-Project': 'attacker'})
        await self.guard.requestheaders(flow)
        await self.guard.request(flow)
        self.assertIsNone(flow.response)
        self.assertEqual(flow.request.host, 'api.openai.com')
        self.assertEqual(flow.request.path, '/v1/responses')
        self.assertEqual(flow.server_conn.address, ('93.184.216.34', 443))
        self.assertEqual(flow.request.headers['Authorization'], 'Bearer synthetic-provider-fixture-secret')
        self.assertNotIn('Cookie', flow.request.headers)
        self.assertNotIn('OpenAI-Project', flow.request.headers)
        flow.response = http.Response.make(200, b'{}')
        await self.guard.response(flow)
        self.assertNotIn('Authorization', flow.request.headers)

    async def test_host_codex_source_routes_and_injects_account_only_on_exact_response_path(self):
        source = Mock()
        source.available.return_value = True
        def route(path):
            if path != '/v1/responses': raise ValueError('unsupported')
            return {'host': 'chatgpt.com', 'path': '/backend-api/codex/responses'}
        source.route.side_effect = route
        source.headers.return_value = {'Authorization': 'Bearer synthetic-host-access-token',
                                       'ChatGPT-Account-ID': 'synthetic-owner-account'}
        self.registry.provider_source = source
        self.registry.bindings['s1']['provider_secret'] = None
        flow = self.flow(headers={'ChatGPT-Account-ID': 'guest-selected-account'})
        await self.guard.request(flow)
        self.assertIsNone(flow.response)
        self.assertEqual(flow.request.host, 'chatgpt.com')
        self.assertEqual(flow.request.path, '/backend-api/codex/responses')
        self.assertEqual(flow.request.headers['ChatGPT-Account-ID'], 'synthetic-owner-account')
        self.assertEqual(flow.request.headers['Authorization'], 'Bearer synthetic-host-access-token')
        source.headers.assert_called_once_with()
        flow.response = http.Response.make(200, b'{}')
        await self.guard.response(flow)
        self.assertNotIn('Authorization', flow.request.headers)
        self.assertNotIn('ChatGPT-Account-ID', flow.request.headers)
        other = self.flow('http://host.docker.internal:19443/openai/v1/chat/completions')
        await self.guard.request(other)
        self.assertEqual(other.response.status_code, 503)

    async def test_host_credential_expiry_on_renew_revokes_lease(self):
        self.registry.provider_source = SimpleNamespace(available=lambda: False)
        self.registry.bindings['s1']['provider_secret'] = None
        self.assertFalse(self.registry.begin(self.value, renew=True)['ready'])
        self.assertIsNone(self.registry.bindings['s1']['lease'])

    async def test_unknown_destination_denied_before_dns_lookup(self):
        flow = self.flow('https://exfiltration.attacker.example/test', 'GET', sni='exfiltration.attacker.example')
        await self.guard.request(flow)
        self.assertEqual(flow.response.status_code, 403)
        self.resolver.assert_not_called()

    async def test_wrong_reverse_path_port_or_authority_denied(self):
        for url in ('http://host.docker.internal:19443/openai/v1/files',
                    'http://host.docker.internal:18765/openai/v1/responses',
                    'http://host.docker.internal:19443/openai/v1/responses?url=evil',
                    'http://host.docker.internal:19443/api/approve'):
            flow = self.flow(url)
            await self.guard.request(flow)
            self.assertEqual(flow.response.status_code, 403)
        self.resolver.assert_not_called()

    async def test_browser_origin_and_opaque_connect_rejected(self):
        flow = self.flow(headers={'Origin': 'http://127.0.0.1:3000'})
        await self.guard.requestheaders(flow)
        self.assertEqual(flow.response.status_code, 403)
        for host, port in (('127.0.0.1', 443), ('example.com', 22), ('swcdn.apple.com', 8443)):
            flow = self.flow('https://' + host + ':' + str(port), method='CONNECT')
            await self.guard.http_connect(flow)
            self.assertEqual(flow.response.status_code, 403)
        self.resolver.assert_not_called()

    async def test_connect_does_not_enable_apple_passthrough_or_raw_tcp(self):
        data = SimpleNamespace(ignore_connection=True, establish_server_tls_first=True,
                               client_hello=SimpleNamespace(sni='swcdn.apple.com'))
        await self.guard.tls_clienthello(data)
        self.assertFalse(data.ignore_connection)
        self.assertFalse(data.establish_server_tls_first)
        flow = SimpleNamespace(kill=lambda: setattr(data, 'killed', True))
        self.guard.tcp_start(flow)
        self.assertTrue(data.killed)

    async def test_end_run_revokes_inflight_decision_before_response_delivery(self):
        flow = self.flow()
        await self.guard.request(flow)
        self.assertIsNone(flow.response)
        self.registry.end(self.value)
        flow.response = http.Response.make(200, b'synthetic upstream result')
        await self.guard.response(flow)
        self.assertEqual(flow.response.status_code, 403)
        self.assertNotIn('Authorization', flow.request.headers)

    async def test_provider_cannot_echo_broker_secret_to_guest(self):
        flow = self.flow()
        await self.guard.request(flow)
        flow.response = http.Response.make(200, b'{"echo":"synthetic-provider-fixture-secret"}')
        await self.guard.response(flow)
        self.assertEqual(flow.response.status_code, 502)
        self.assertNotIn(b'synthetic-provider-fixture-secret', flow.response.content)
        self.assertNotIn('Authorization', flow.request.headers)

    async def test_provider_stream_inherits_broker_authorization_and_cleanup(self):
        flow = self.flow()
        await self.guard.requestheaders(flow)
        await self.guard.request(flow)
        self.assertIsNone(flow.response)
        self.assertIs(flow.request.stream, False)
        self.assertEqual(flow.request.headers['Accept-Encoding'], 'identity')
        flow.response = http.Response.make(200, b'', {'Content-Type':'text/event-stream'})
        flow.response.headers.pop('Content-Length', None)
        await self.guard.responseheaders(flow)
        self.assertTrue(callable(flow.response.stream))
        self.assertEqual(flow.response.stream(b'data: hello\n\n'), b'data: hello\n\n')
        self.assertIn(flow.id, self.guard.tasks)
        flow.response.stream(b'')
        await self.guard.response(flow)
        self.assertIsNone(flow.error)
        self.assertNotIn('Authorization', flow.request.headers)
        self.assertNotIn(flow.id, self.guard.tasks)
        self.assertFalse(self.guard.streams)
        self.assertFalse(self.registry.bindings['s1']['decisions'])

    async def test_provider_stream_blocks_split_broker_credential(self):
        flow = self.flow()
        await self.guard.request(flow)
        flow.response = http.Response.make(200, b'', {'Content-Type':'text/event-stream'})
        flow.response.headers.pop('Content-Length', None)
        await self.guard.responseheaders(flow)
        self.assertEqual(flow.response.stream(b'data: synthetic-provider-fixture-'), b'data: ')
        self.assertEqual(flow.response.stream(b'secret\n\n'), [])
        await asyncio.gather(*self.guard.stream_cleanup)
        self.assertIsNotNone(flow.error)
        self.assertNotIn('Authorization', flow.request.headers)
        self.assertFalse(self.guard.streams)
        self.assertFalse(self.registry.bindings['s1']['decisions'])

    async def test_end_run_interrupts_active_provider_stream(self):
        flow = self.flow()
        await self.guard.request(flow)
        flow.response = http.Response.make(200, b'', {'Content-Type':'text/event-stream'})
        flow.response.headers.pop('Content-Length', None)
        await self.guard.responseheaders(flow)
        callback = flow.response.stream
        self.assertEqual(callback(b'data: hello\n\n'), b'data: hello\n\n')
        self.registry.end(self.value)
        await asyncio.wait_for(self.guard.tasks[flow.id], 2)
        await asyncio.gather(*self.guard.stream_cleanup)
        self.assertEqual(callback(b'data: too late\n\n'), [])
        self.assertIsNotNone(flow.error)
        self.assertNotIn('Authorization', flow.request.headers)
        self.assertFalse(self.guard.streams)
        self.assertFalse(self.registry.bindings['s1']['decisions'])

    async def test_lost_control_never_forwards_or_resolves(self):
        self.guard.capability = 'wrong'
        flow = self.flow()
        await self.guard.request(flow)
        self.assertEqual(flow.response.status_code, 503)
        self.resolver.assert_not_called()

    def configure_document_api(self):
        from warden.sbx_documents import DocumentAPI
        key = Path(self.tmp.name) / 'document-key'
        key.write_text('synthetic-document-key-at-least-32-characters')
        key.chmod(0o600)
        self.registry.document_api = DocumentAPI('http://localhost:18080', key)
        self.registry.document_api.dispatch = Mock(return_value={'status': 200, 'body': 'eyJkb2N1bWVudHMiOltdfQ=='})
        return self.registry.document_api

    async def test_documents_use_broker_http_strip_guest_headers_and_keep_signatures_off_flow(self):
        api = self.configure_document_api()
        flow = self.flow('http://host.docker.internal:19443/workspace/v1/documents', 'GET',
            headers={'Cookie': 'owner=forged', 'X-Warden-Context': 'forged', 'X-Warden-Signature': 'forged',
                     'X-Forwarded-Host': 'attacker', 'Proxy-Authorization': 'Bearer forged'})
        flow.request.content = b''
        await self.guard.requestheaders(flow)
        await self.guard.request(flow)
        await self.guard.responseheaders(flow)
        await self.guard.response(flow)
        self.assertEqual(flow.response.status_code, 200)
        self.assertEqual(flow.response.content, b'{"documents":[]}')
        self.assertEqual(list(flow.request.headers), [])
        self.assertFalse(flow.response.stream)
        self.assertFalse(self.guard.streams)
        self.resolver.assert_not_called()
        method, path, body, headers = api.dispatch.call_args.args
        self.assertEqual((method, path, body), ('GET', '/agent/v1/documents', b''))
        self.assertNotEqual(headers['X-Warden-Signature'], 'forged')
        self.assertNotIn('Authorization', headers)
        self.assertNotIn('Cookie', headers)
        self.assertNotIn(headers['X-Warden-Signature'].encode(), flow.response.content)

    async def test_document_bad_route_authority_origin_encoding_and_missing_lease_denied(self):
        api = self.configure_document_api()
        for url, headers in (
            ('http://host.docker.internal:19443/workspace/v1/documents?projectID=other', {}),
            ('http://host.docker.internal:19444/workspace/v1/documents', {}),
            ('https://host.docker.internal:19443/workspace/v1/documents', {}),
            ('http://host.docker.internal:19443/workspace/v1/documents', {'Content-Encoding': 'gzip'}),
            ('http://host.docker.internal:19443/workspace/v1/documents', {'Origin': 'http://localhost:18080'}),
        ):
            flow = self.flow(url, 'GET', headers=headers)
            flow.request.content = b''
            await self.guard.requestheaders(flow)
            await self.guard.request(flow)
            self.assertGreaterEqual(flow.response.status_code, 400)
        self.registry.end(self.value)
        flow = self.flow('http://host.docker.internal:19443/workspace/v1/documents', 'GET')
        flow.request.content = b''
        await self.guard.request(flow)
        self.assertEqual(flow.response.status_code, 503)
        api.dispatch.assert_not_called()
        self.resolver.assert_not_called()

    async def test_document_broker_loss_fails_closed(self):
        api = self.configure_document_api()
        self.guard.capability = 'forged'
        flow = self.flow('http://host.docker.internal:19443/workspace/v1/documents', 'GET')
        flow.request.content = b''
        await self.guard.request(flow)
        self.assertEqual(flow.response.status_code, 503)
        api.dispatch.assert_not_called()
        self.resolver.assert_not_called()

    def test_gateway_launcher_pins_local_inspected_options(self):
        from warden.sbx_gateway import command
        cmd = command('/fixture/mitmdump', Path(self.tmp.name), 19443)
        self.assertEqual(cmd[cmd.index('--mode') + 1], 'regular')
        self.assertEqual(cmd[cmd.index('--listen-host') + 1], '127.0.0.1')
        for option in ('rawtcp=false', 'websocket=false', 'ssl_insecure=false', 'connection_strategy=lazy'):
            self.assertIn(option, cmd)

    async def test_claude_lease_routes_only_claude_and_strips_guest_credentials(self):
        self.registry.end(self.value)
        value={**self.value,'runID':'claude-run','provider':'claude'}
        def route(path):
            if path not in ('/v1/messages','/v1/messages/count_tokens'):raise ValueError('wrong provider')
            return {'host':'api.anthropic.com','path':path}
        self.registry.claude_source=SimpleNamespace(available=lambda:True,route=route,headers=lambda:{'Authorization':'Bearer synthetic-claude-host-secret'})
        self.assertTrue(self.registry.begin(value)['ready'])
        flow=self.flow('http://host.docker.internal:19443/anthropic/v1/messages?beta=true',headers={'x-api-key':'guest-key','Cookie':'guest-cookie'})
        await self.guard.requestheaders(flow);await self.guard.request(flow)
        self.assertIsNone(flow.response)
        self.assertEqual(flow.request.host,'api.anthropic.com')
        self.assertEqual(flow.request.headers['Authorization'],'Bearer synthetic-claude-host-secret')
        self.assertNotIn('x-api-key',flow.request.headers);self.assertNotIn('Cookie',flow.request.headers)
        other=self.flow();await self.guard.request(other);self.assertIsNotNone(other.response)
        self.registry.end(value)
        denied=self.flow('http://host.docker.internal:19443/anthropic/v1/messages');await self.guard.request(denied)
        self.assertIsNotNone(denied.response)


    async def test_google_sharing_injection_and_revocation(self):
        from warden.sharing import Sharing
        google = Mock()
        google.file.return_value = {'id':'doc-a','title':'Plan'}
        google.authorization.return_value = 'Bearer synthetic-google-credential'
        sharing = Sharing(self.tmp.name, google)
        self.registry.sharing = sharing
        r = sharing.dispatch('request', {'chatID':self.value['chatID'],'sandboxID':self.value['sandboxID'],'callID':'tool-1','reason':'Read plan'})
        sharing.dispatch('resolve', {'id':r['request_id'],'allow':True,'documents':['doc-a'],'duration':900})
        flow = self.flow('https://docs.googleapis.com/v1/documents/doc-a', 'GET', {'Cookie':'hostile-cookie','Host':'docs.googleapis.com'}, sni='docs.googleapis.com')
        flow.request.content=b''
        await self.guard.requestheaders(flow)
        await self.guard.request(flow)
        self.assertIsNone(flow.response, flow.response.text if flow.response else '')
        self.assertEqual(flow.request.headers['Authorization'],'Bearer synthetic-google-credential')
        self.assertNotIn('Cookie',flow.request.headers)
        await asyncio.sleep(0)
        self.assertFalse(self.guard.tasks[flow.id].done())
        sharing.dispatch('revoke',{'id':r['request_id']})
        flow.response = http.Response.make(200,b'{"documentId":"doc-a"}')
        await self.guard.response(flow)
        self.assertNotEqual(flow.response.status_code,200)
        denied = self.flow('https://docs.googleapis.com/v1/documents/doc-b', 'GET', {'Host':'docs.googleapis.com'}, sni='docs.googleapis.com')
        denied.request.content=b''
        await self.guard.requestheaders(denied)
        await self.guard.request(denied)
        self.assertEqual(denied.response.status_code,403)
        self.assertNotIn('Authorization',denied.request.headers)
        sharing.db.close()


    async def test_repository_sharing_injection_and_revocation(self):
        from warden.sharing import Sharing
        github = Mock(owner='owner', app_id=123)
        github.repositories.return_value = {'repositories':[{'id':9,'full_name':'owner/repo'}],'next_page':None}
        github.authorization.return_value = 'Bearer synthetic-github-credential'
        sharing = Sharing(self.tmp.name, github=github)
        self.registry.sharing = sharing
        data = {'chatID':self.value['chatID'],'sandboxID':self.value['sandboxID']}
        sharing.dispatch('github_select', {**data,'repositories':['owner/repo']})
        flow = self.flow('https://api.github.com/repos/owner/repo', 'GET', {'Cookie':'forged','Host':'api.github.com','Authorization':'Bearer forged'}, sni='api.github.com')
        flow.request.content=b''
        await self.guard.requestheaders(flow)
        await self.guard.request(flow)
        self.assertIsNone(flow.response, flow.response.text if flow.response else '')
        self.assertEqual(flow.request.headers['Authorization'],'Bearer synthetic-github-credential')
        self.assertNotIn('Cookie',flow.request.headers)
        await asyncio.sleep(0)
        self.assertFalse(self.guard.tasks[flow.id].done())
        sharing.dispatch('github_select',{**data,'repositories':[]})
        flow.response = http.Response.make(200,b'{"full_name":"owner/repo"}')
        await self.guard.response(flow)
        self.assertNotEqual(flow.response.status_code,200)
        denied = self.flow('https://api.github.com/repos/owner/repo', 'GET', {'Host':'api.github.com'}, sni='api.github.com')
        denied.request.content=b''
        await self.guard.requestheaders(denied)
        await self.guard.request(denied)
        self.assertFalse(denied.response is None)
        self.assertNotEqual(denied.request.headers.get('Authorization'),'Bearer synthetic-github-credential')
        self.assertGreaterEqual(denied.response.status_code,400)
        sharing.db.close()
