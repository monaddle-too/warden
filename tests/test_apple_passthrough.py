"""Apple's exception must never trust the guest's destination or fail open."""
import asyncio
import importlib.util
import socket
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch
try:
    from mitmproxy import connection, http
    HAVE_MITM = True
except ImportError:
    HAVE_MITM = False

@unittest.skipUnless(HAVE_MITM, 'requires mitmproxy')
class ApplePassthroughTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        spec = importlib.util.spec_from_file_location('apple_addon', Path(__file__).resolve().parents[1]/'proxy/addon.py')
        self.module = importlib.util.module_from_spec(spec); spec.loader.exec_module(self.module)
        self.guard = self.module.Guard()
        self.resolve = patch.object(asyncio.get_running_loop(), 'getaddrinfo', AsyncMock(return_value=[
            (socket.AF_INET, socket.SOCK_STREAM, 6, '', ('17.253.5.10', 443))]))
        self.resolve.start()
        self.audit = AsyncMock(return_value={'recorded':True,'allow':True,'decision_id':'egress','request_id':'r','remaining_seconds':3600})
        self.rpc = patch.object(self.module, 'call', self.audit); self.rpc.start()
    def tearDown(self):
        self.guard.done(); self.rpc.stop(); self.resolve.stop()
    def hello(self, host='swscan.apple.com', target=('140.82.116.5',443)):
        client = connection.Client(peername=('10.77.0.73',1234), sockname=target)
        server = connection.Server(address=target)
        return SimpleNamespace(client_hello=SimpleNamespace(sni=host),
            context=SimpleNamespace(client=client, server=server), ignore_connection=False)
    async def test_allowed_sni_reroutes_forged_github_destination(self):
        data = self.hello()
        await self.guard.tls_clienthello(data)
        self.assertTrue(data.ignore_connection)
        self.assertEqual(data.context.server.address, ('17.253.5.10',443))
        self.assertIsNone(data.context.server.error)
        self.assertEqual(self.audit.call_args.args[0]['event_type'],'tls.passthrough')
        from warden.server import internal
        self.assertTrue(internal(Mock(), self.audit.call_args.args[0])['recorded'])
        self.guard.server_connect(data.context)
        self.assertIsNone(data.context.server.error)
        data.context.server.address = ('140.82.116.5',443)
        self.guard.server_connect(data.context)
        self.assertIsNotNone(data.context.server.error)
        self.guard.client_disconnected(data.context.client)
        self.assertFalse(self.guard.apple_tunnels)
    async def test_exact_hostnames_only(self):
        for name in [None,'api.github.com','github.com','swscan.apple.com.evil.test',
                     'evil-swscan.apple.com','swscan.apple.com.','17.253.5.10','gsa.apple.com','www.apple.com']:
            data = self.hello(name)
            await self.guard.tls_clienthello(data)
            self.assertFalse(data.ignore_connection, name)
        self.audit.assert_not_awaited()
    async def test_unsafe_dns_and_ports_fail_closed(self):
        for addresses in [['127.0.0.1'],['140.82.116.5'],['17.253.5.10','10.0.0.1'],[],['2606:4700:4700::1111']]:
            asyncio.get_running_loop().getaddrinfo.return_value = [
                (socket.AF_INET, socket.SOCK_STREAM,6,'',(address,443)) for address in addresses]
            data = self.hello()
            await self.guard.tls_clienthello(data)
            self.assertTrue(data.ignore_connection)
            self.assertIsNotNone(data.context.server.error, addresses)
            self.assertFalse(self.guard.apple_tunnels)
        data = self.hello(target=('17.253.5.10',80))
        await self.guard.tls_clienthello(data)
        self.assertIsNotNone(data.context.server.error)
    async def test_dns_and_audit_failures_keep_deny_state(self):
        for failure in ['dns','audit']:
            if failure == 'dns': asyncio.get_running_loop().getaddrinfo.side_effect = OSError('dns unavailable')
            else:
                asyncio.get_running_loop().getaddrinfo.side_effect = None
                self.audit.side_effect = OSError('audit unavailable')
            data = self.hello()
            await self.guard.tls_clienthello(data)
            self.assertIsNotNone(data.context.server.error)
            self.assertFalse(self.guard.apple_tunnels)
    async def test_early_server_connection_cannot_get_exception(self):
        data = self.hello()
        data.context.server.state = connection.ConnectionState.OPEN
        await self.guard.tls_clienthello(data)
        self.assertFalse(data.ignore_connection)
    async def test_apple_exception_obeys_host_destination_policy(self):
        self.audit.return_value={'allow':False}
        data=self.hello()
        await self.guard.tls_clienthello(data)
        self.assertIsNotNone(data.context.server.error)
        self.assertFalse(self.guard.apple_tunnels)
    async def test_cleartext_package_upgrades_only_to_fixed_https_host(self):
        for host,target in self.module.APPLE_DOWNLOAD_HTTPS.items():
            data = self.hello(host, ('17.253.5.10',80))
            flow = http.HTTPFlow(data.context.client, data.context.server)
            flow.request = http.Request.make('GET',f'http://{host}/content/package.pkg?part=1', headers={'Host':host})
            await self.guard.request(flow)
            self.assertEqual(flow.response.status_code,307, flow.response.text)
            self.assertEqual(flow.response.headers['Location'],f'https://{target}/content/package.pkg?part=1')
    async def test_protected_resolution_cannot_get_download_redirect(self):
        asyncio.get_running_loop().getaddrinfo.return_value = [
            (socket.AF_INET,socket.SOCK_STREAM,6,'',('140.82.116.5',80))]
        self.audit.return_value = {'allow':False,'status':403,'reason':'protected destination'}
        data = self.hello('swcdn.apple.com',('140.82.116.5',80))
        flow = http.HTTPFlow(data.context.client,data.context.server)
        flow.request = http.Request.make('GET','http://swcdn.apple.com/content/package.pkg', headers={'Host':'swcdn.apple.com'})
        await self.guard.request(flow)
        self.assertEqual(flow.response.status_code,403)

class AppleAuditTests(unittest.TestCase):
    def test_passthrough_is_network_activity_without_bodies(self):
        from warden.ocsf import Exporter
        event = {'schema_version':'1.0.0','event_type':'tls.passthrough','event_id':'test',
            'time':'2026-09-10T00:00:00Z','producer':{'name':'warden','instance_id':'i','version':'0.1'},
            'hostname':'swscan.apple.com','destination':{'DST':'17.253.5.10','DPT':443,'PROTO':'TCP'}}
        result = Exporter().convert(event)
        self.assertEqual(result['class_uid'],4001)
        self.assertEqual(result['activity_id'],1)
        self.assertEqual(result['dst_endpoint']['hostname'],'swscan.apple.com')
