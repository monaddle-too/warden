import http.client
from http.server import ThreadingHTTPServer
from pathlib import Path
import sys
import threading
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]/'proxy'))
import assets
ID='cda2eb60-4c93-4e32-ae9e-67605a83d1d4'
HEADERS={'X-Warden-Clipboard':'1','X-Warden-Clipboard-Version':'2'}

class ClipboardProxyTests(unittest.TestCase):
    def setUp(self):
        self.server=assets.Server(('127.0.0.1',0),assets.Handler)
        threading.Thread(target=self.server.serve_forever,daemon=True).start()
    def tearDown(self): self.server.shutdown(); self.server.server_close()
    def request(self, path='/clipboard', headers=None, method='GET'):
        conn=http.client.HTTPConnection(*self.server.server_address)
        conn.request(method,path,headers=headers or {})
        response=conn.getresponse(); result=(response.status,dict(response.getheaders()),response.read())
        conn.close(); return result
    def test_fixed_route_cors_and_methods(self):
        with patch.object(assets,'take',return_value=b'private') as take:
            for path,headers,method in [('/clipboard',{},'GET'),('/clipboard',{'X-Warden-Clipboard':'1','Origin':'https://evil.example'},'GET'),('/clipboard?rpc=authorize',{'X-Warden-Clipboard':'1'},'GET'),('/clipboard',{'X-Warden-Clipboard':'1'},'POST')]:
                self.assertGreaterEqual(self.request(path,headers,method)[0],400)
            take.assert_not_called()
    def test_plain_utf8_no_cache_and_no_transfer(self):
        text='line one\n世界 🌍\r\n{\\rtf1 literal}'
        with patch.object(assets,'take',return_value=(text.encode(),ID)):
            status,headers,data=self.request(headers=HEADERS)
        self.assertEqual(status,200); self.assertEqual(data.decode(),text)
        self.assertEqual(headers['Cache-Control'],'no-store')
        self.assertEqual(headers['Content-Type'],'text/plain; charset=utf-8')
        with patch.object(assets,'take',return_value=None):
            self.assertEqual(self.request(headers=HEADERS)[::2],(204,b''))
    def test_bridge_failure_does_not_leak_exception(self):
        with patch.object(assets,'take',side_effect=ValueError('secret-value')):
            status,_,data=self.request(headers=HEADERS)
        self.assertEqual(status,503); self.assertNotIn(b'secret-value',data)
    def test_ack_is_fixed_bounded_route(self):
        with patch.object(assets,'acknowledge',return_value=True) as ack:
            headers={**HEADERS,'X-Warden-Transfer':ID,'Content-Length':'0'}
            self.assertEqual(self.request('/clipboard/ack',headers,'POST')[0],204)
            ack.assert_called_once_with(ID); ack.reset_mock()
            for overrides in [{'Origin':'https://evil.example'},{'X-Warden-Transfer':'bad'},{'Content-Length':'1'},{'Transfer-Encoding':'chunked'}]:
                self.assertGreaterEqual(self.request('/clipboard/ack',{**headers,**overrides},'POST')[0],400)
            ack.assert_not_called()
    def test_old_receiver_cannot_consume_new_transfer(self):
        with patch.object(assets,'take') as take:
            self.assertEqual(self.request(headers={'X-Warden-Clipboard':'1'})[0],426)
            take.assert_not_called()
