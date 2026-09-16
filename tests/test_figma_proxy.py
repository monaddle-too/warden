import asyncio
import socket
import unittest
from unittest.mock import patch
import test_proxy


@unittest.skipUnless(test_proxy.HAVE_MITM, 'requires mitmproxy')
class FigmaProxyTests(unittest.IsolatedAsyncioTestCase):
    setUp = test_proxy.ProxyTests.setUp
    tearDown = test_proxy.ProxyTests.tearDown
    flow = test_proxy.ProxyTests.flow

    async def test_figma_uses_protected_route_on_non_github_network(self):
        asyncio.get_event_loop().getaddrinfo.return_value = [(socket.AF_INET,socket.SOCK_STREAM,6,'',('104.18.10.10',443))]
        flow = self.flow(url='https://api.figma.com/v1/files/TestFile',sni='api.figma.com',method='GET',body=b'',
                         headers={'Authorization':'Bearer guest','X-Figma-Token':'guest-pat','Cookie':'session=guest'})
        actions = []
        async def dispatch(message):
            actions.append(message)
            if message['action'] == 'authorize':
                self.assertEqual(message['request']['headers'],[])
                return {'allow':True,'authorization':'Bearer figma_test_secret','decision_id':'d','request_id':'r','remaining_seconds':60}
            return {'active':True,'recorded':True}
        with patch.object(self.module,'call',dispatch):
            await self.guard.request(flow)
        self.assertIsNone(flow.response)
        self.assertEqual(flow.request.headers['Authorization'],'Bearer figma_test_secret')
        self.assertNotIn('X-Figma-Token',flow.request.headers)
        self.assertNotIn('Cookie',flow.request.headers)
        self.assertNotIn('Content-Length',flow.request.headers)
        self.assertEqual(flow.server_conn.sni,'api.figma.com')
        self.assertFalse(any(m['action']=='egress' for m in actions))
        from mitmproxy import http
        flow.response = http.Response.make(200,b'{"name":"Test"}',{'Content-Type':'application/json'})
        with patch.object(self.module,'call',dispatch): await self.guard.response(flow)
        self.assertNotIn('Authorization',flow.request.headers)
        self.assertEqual(flow.response.status_code,200)

    async def test_no_approval_never_dispatches(self):
        asyncio.get_event_loop().getaddrinfo.return_value = [(socket.AF_INET,socket.SOCK_STREAM,6,'',('104.18.10.10',443))]
        flow = self.flow(url='https://api.figma.com/v1/files/TestFile',sni='api.figma.com',method='GET',body=b'')
        async def dispatch(message):
            if message['action']=='authorize': return {'allow':False,'status':428,'reason':'approval required'}
            return {'recorded':True}
        with patch.object(self.module,'call',dispatch): await self.guard.request(flow)
        self.assertEqual(flow.response.status_code,428)
        self.assertNotIn('Authorization',flow.request.headers)

    async def test_unsupported_figma_channels_cannot_use_public_egress(self):
        asyncio.get_event_loop().getaddrinfo.return_value = [(socket.AF_INET,socket.SOCK_STREAM,6,'',('104.18.10.10',443))]
        for host in ('www.figma.com','mcp.figma.com','api.figma-gov.com'):
            flow=self.flow(url='https://'+host+'/v1/me',sni=host,method='GET',body=b'')
            async def dispatch(message):
                self.assertNotEqual(message['action'],'egress')
                return {'allow':False,'status':403,'reason':'unsupported channel'}
            with patch.object(self.module,'call',dispatch): await self.guard.request(flow)
            self.assertEqual(flow.response.status_code,403)
