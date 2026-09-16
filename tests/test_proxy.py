"""Real mitmproxy flow objects; no network or real GitHub token required."""
import asyncio
import importlib.util
from pathlib import Path
import socket
from urllib.parse import urlsplit
import unittest
from unittest.mock import AsyncMock, patch
from types import SimpleNamespace

try:
    from mitmproxy import http, connection
    HAVE_MITM=True
except ImportError: HAVE_MITM=False

@unittest.skipUnless(HAVE_MITM,'install proxy dependencies to run flow tests')
class ProxyTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        spec=importlib.util.spec_from_file_location('warden_test_addon',Path(__file__).resolve().parents[1]/'proxy/addon.py')
        self.module=importlib.util.module_from_spec(spec); spec.loader.exec_module(self.module)
        self.guard=self.module.Guard()
        self.resolver=patch.object(asyncio.get_event_loop(),'getaddrinfo',new=AsyncMock(return_value=[(socket.AF_INET,socket.SOCK_STREAM,6,'',('140.82.116.5',443))])); self.resolver.start()
    def tearDown(self): self.guard.done(); self.resolver.stop()
    def flow(self,url='https://api.github.com/repos/acme/demo/pulls',method='POST',body=b'{"title":"change"}',headers=None,sni='api.github.com'):
        client=connection.Client(peername=('10.77.0.2',23456),sockname=('140.82.116.5',443),sni=sni)
        server=connection.Server(address=('140.82.116.5',443))
        flow=http.HTTPFlow(client,server,live=True)
        flow.request=http.Request.make(method,url,body,{'Host':urlsplit(url).netloc, **(headers or {'Content-Type':'application/json'})})
        return flow
    async def test_no_approval_no_token_injection(self):
        flow=self.flow(headers={'Content-Type':'application/json','Authorization':'Bearer guest-token'})
        mock=AsyncMock(return_value={'allow':False,'status':428,'reason':'approval required','request_id':'r'})
        with patch.object(self.module,'call',mock): await self.guard.request(flow)
        self.assertEqual(flow.response.status_code,428)
        submitted=mock.call_args_list[0].args[0]['request']
        self.assertFalse(any(k.lower()=='authorization' for k,v in submitted['headers']))
    async def test_authorized_request_gets_host_credential_only(self):
        flow=self.flow(headers={'Content-Type':'application/json','Authorization':'Bearer guest','Cookie':'evil=1'})
        async def dispatch(message):
            if message['action']=='authorize': return {'allow':True,'authorization':'Bearer ghp_'+'test'*12,'decision_id':'d','request_id':'r','remaining_seconds':60,'expires_at':9999999999}
            return {'active':True}
        with patch.object(self.module,'call',dispatch): await self.guard.request(flow)
        self.assertIsNone(flow.response)
        self.assertEqual(flow.request.headers['Authorization'],'Bearer ghp_'+'test'*12)
        self.assertNotIn('Cookie',flow.request.headers)
        self.assertEqual(flow.server_conn.address,('140.82.116.5',443)); self.assertEqual(flow.server_conn.sni,'api.github.com')
    async def test_sni_host_mismatch_denied(self):
        flow=self.flow(sni='example.com'); await self.guard.request(flow); self.assertEqual(flow.response.status_code,403)
    async def test_private_rebinding_denied(self):
        asyncio.get_event_loop().getaddrinfo.return_value=[(socket.AF_INET,socket.SOCK_STREAM,6,'',('127.0.0.1',443))]
        flow=self.flow(); await self.guard.request(flow); self.assertEqual(flow.response.status_code,403)
    async def test_raw_ip_and_upgrade_denied(self):
        flow=self.flow(url='https://140.82.116.5/user',sni=None); await self.guard.request(flow); self.assertEqual(flow.response.status_code,403)
        flow=self.flow(headers={'Upgrade':'websocket'}); await self.guard.requestheaders(flow); self.assertEqual(flow.response.status_code,403)
    async def test_control_failure_is_closed(self):
        flow=self.flow()
        with patch.object(self.module,'call',AsyncMock(side_effect=OSError())): await self.guard.request(flow)
        self.assertEqual(flow.response.status_code,503)
    async def test_external_audit_scrubs_credentials(self):
        asyncio.get_event_loop().getaddrinfo.return_value=[(socket.AF_INET,socket.SOCK_STREAM,6,'',('93.184.215.14',443))]
        flow=self.flow(url='https://example.com/?token=hidden',sni='example.com',headers={'Authorization':'Bearer external-secret','Content-Type':'application/json'})
        mock=AsyncMock(return_value={'recorded':True,'allow':True,'decision_id':'external','request_id':'r','remaining_seconds':3600})
        with patch.object(self.module,'call',mock): await self.guard.request(flow)
        self.assertIsNone(flow.response)
        self.assertNotIn('external-secret',str(mock.call_args)); self.assertNotIn('hidden',str(mock.call_args))
        self.assertEqual(flow.request.headers['Authorization'],'Bearer external-secret')
    async def test_response_withheld_if_audit_fails(self):
        flow=self.flow(); flow.response=http.Response.make(200,b'hello')
        with patch.object(self.module,'call',AsyncMock(side_effect=OSError())): await self.guard.response(flow)
        self.assertEqual(flow.response.status_code,503)
    async def test_expired_dispatch_does_not_retain_host_credential(self):
        flow=self.flow()
        async def dispatch(message):
            if message['action']=='authorize':return {'allow':True,'authorization':'Bearer ghp_'+'test'*12,'decision_id':'d','request_id':'r','remaining_seconds':60}
            if message['action']=='active':return {'active':False}
            return {'recorded':True}
        with patch.object(self.module,'call',dispatch):await self.guard.request(flow)
        self.assertEqual(flow.response.status_code,403)
        self.assertNotIn('Authorization',flow.request.headers)

    async def test_transport_errors_use_accepted_audit_fields_and_scrub_secrets(self):
        from warden.server import internal
        secret='ghp_'+'secret'*8
        self.guard.redactor.register(secret)
        mock=AsyncMock(return_value={'recorded':True})
        data=SimpleNamespace(conn=SimpleNamespace(sni='www.facebook.com',error='certificate unknown '+secret))
        with patch.object(self.module,'call',mock): await self.guard.tls_failed_client(data)
        message=mock.call_args.args[0]
        self.assertNotIn(secret,str(message))
        from unittest.mock import Mock
        engine=Mock()
        self.assertTrue(internal(engine,message)['recorded'])
        self.assertIn('certificate unknown',message['fields']['reason'])

    async def test_external_response_clears_unsupported_alternative_transport(self):
        flow=self.flow(url='https://example.com/',sni='example.com')
        flow.response=http.Response.make(200,b'hello',{'Alt-Svc':'h3=":443"; ma=86400'})
        with patch.object(self.module,'call',AsyncMock(return_value={'recorded':True})): await self.guard.response(flow)
        self.assertEqual(flow.response.status_code,200)
        self.assertEqual(flow.response.headers['Alt-Svc'],'clear')

    async def test_http2_cookie_fragments_are_preserved_and_redacted(self):
        asyncio.get_event_loop().getaddrinfo.return_value=[(socket.AF_INET,socket.SOCK_STREAM,6,'',('93.184.215.14',443))]
        flow=self.flow(url='https://www.google.com/',sni='www.google.com')
        flow.request.http_version='HTTP/2.0'
        flow.request.headers.set_all('cookie',['first=private-one','second=private-two'])
        mock=AsyncMock(return_value={'recorded':True,'allow':True,'decision_id':'external','request_id':'r','remaining_seconds':3600})
        with patch.object(self.module,'call',mock): await self.guard.request(flow)
        self.assertIsNone(flow.response)
        self.assertEqual(flow.request.headers.get_all('cookie'),['first=private-one; second=private-two'])
        self.assertNotIn('private-one',str(mock.call_args)); self.assertNotIn('private-two',str(mock.call_args))

    async def test_http2_duplicate_authorization_still_denied_and_audited(self):
        from warden.server import internal
        from unittest.mock import Mock
        flow=self.flow()
        flow.request.http_version='HTTP/2.0'
        flow.request.headers.set_all('authorization',['Bearer private-one','Bearer private-two'])
        mock=AsyncMock(return_value={'recorded':True,'allow':True,'decision_id':'external','request_id':'r','remaining_seconds':3600})
        with patch.object(self.module,'call',mock): await self.guard.request(flow)
        self.assertEqual(flow.response.status_code,403)
        self.assertNotIn('approval',flow.response.json())
        event=mock.call_args.args[0]
        self.assertEqual(event['fields']['request_id'],flow.response.json()['request_id'])
        self.assertEqual(event['fields']['reason'],'duplicate headers unsupported')
        self.assertEqual(event['fields']['request']['header_names'].count('authorization'),2)
        self.assertNotIn('private-one',str(event)); self.assertNotIn('private-two',str(event))
        self.assertTrue(internal(Mock(),event)['recorded'])

    async def test_github_http2_cookies_never_reach_authorization(self):
        flow=self.flow(); flow.request.http_version='HTTP/2.0'
        flow.request.headers.set_all('cookie',['a=private-one','b=private-two'])
        mock=AsyncMock(return_value={'allow':False,'status':428,'reason':'approval required','request_id':'r'})
        with patch.object(self.module,'call',mock): await self.guard.request(flow)
        self.assertEqual(flow.response.status_code,428)
        submitted=mock.call_args_list[0].args[0]['request']
        self.assertFalse(any(k.lower()=='cookie' for k,v in submitted['headers']))
        self.assertNotIn('private-one',str(mock.call_args_list))

    async def test_early_upgrade_rejection_has_audit_event(self):
        flow=self.flow(headers={'Upgrade':'websocket'})
        mock=AsyncMock(return_value={'recorded':True})
        with patch.object(self.module,'call',mock): await self.guard.requestheaders(flow)
        self.assertEqual(flow.response.status_code,403)
        self.assertEqual(mock.call_args.args[0]['fields']['status'],403)
