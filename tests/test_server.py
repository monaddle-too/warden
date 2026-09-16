import http.client
import json
from pathlib import Path
import tempfile
import threading
import unittest
from warden.core import Engine
from warden.server import HTTPServer, make_handler, internal

class ServerTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory(); self.engine=Engine(self.tmp.name)
        self.server=HTTPServer(('127.0.0.1',0),make_handler(self.engine,'test-session',0))
        self.port=self.server.server_address[1]
        self.server.RequestHandlerClass=make_handler(self.engine,'test-session',self.port)
        self.thread=threading.Thread(target=self.server.serve_forever,daemon=True); self.thread.start()
    def tearDown(self):
        self.engine.clipboard.cancel()
        self.server.shutdown(); self.server.server_close(); self.engine.db.close()
        import os
        os.close(self.engine.audit.fd); self.tmp.cleanup()
    def request(self,path,method='GET',headers=None,body=None):
        conn=http.client.HTTPConnection('127.0.0.1',self.port)
        conn.request(method,path,body=body,headers=headers or {}); response=conn.getresponse(); status=response.status; content=response.read(); conn.close(); return status,content
    def test_ui_assets_and_authentication(self):
        status,html=self.request('/')
        self.assertEqual(status,200)
        self.assertNotIn(b'clipboard-form',html)
        self.assertNotIn(b'Send text to VM',html)
        self.assertEqual(self.request('/api/state')[0],401)
        self.assertEqual(self.request('/api/state',headers={'Authorization':'Bearer test-session'})[0],200)
    def test_resources_are_authenticated_and_never_block_on_sampling(self):
        from warden.metrics import ResourceMonitor
        self.engine.resources = ResourceMonitor(self.engine.state)
        status, body = self.request('/api/state', headers={'Authorization':'Bearer test-session'})
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body)['resources'], {'macos':{'status':'sampling'}, 'proxy':{'status':'sampling'}})

    def test_history_and_traffic_pages_require_host_authentication(self):
        for endpoint in ['/api/history','/api/traffic']:
            self.assertEqual(self.request(endpoint)[0],401)
            status,body=self.request(endpoint,headers={'Authorization':'Bearer test-session'})
            self.assertEqual(status,200)
            self.assertEqual(json.loads(body)['page_size'],25)
            self.assertEqual(self.request(endpoint+'?cursor=invalid',headers={'Authorization':'Bearer test-session'})[0],400)
        self.assertEqual(self.request('/api/traffic?path=/etc/passwd',headers={'Authorization':'Bearer test-session'})[0],400)

    def test_dns_rebinding_host_rejected(self):
        self.assertEqual(self.request('/api/state',headers={'Host':'evil.example','Authorization':'Bearer test-session'})[0],403)
    def test_cross_origin_mutation_rejected(self):
        headers={'Authorization':'Bearer test-session','Content-Type':'application/json','Origin':'https://evil.example'}
        self.assertEqual(self.request('/api/policy','POST',headers,json.dumps(self.engine.policy))[0],403)
    def test_policy_mutation_requires_correct_origin_and_session(self):
        headers={'Authorization':'Bearer test-session','Content-Type':'application/json','Origin':f'http://127.0.0.1:{self.port}'}
        self.assertEqual(self.request('/api/policy','POST',headers,json.dumps(self.engine.policy))[0],200)
        headers.pop('Authorization'); self.assertEqual(self.request('/api/policy','POST',headers,json.dumps(self.engine.policy))[0],403)
    def test_proxy_cannot_approve_or_set_token(self):
        for action in ['approve','set_token','revoke','policy','exec','request-review','git-review']:
            with self.assertRaises(ValueError): internal(self.engine,{'action':action})
    def test_emergency_controls_and_deep_links_require_host_authentication(self):
        headers={'Authorization':'Bearer test-session','Content-Type':'application/json','Origin':f'http://127.0.0.1:{self.port}'}
        for endpoint,body in [('/api/network','{"enabled":false}'),('/api/revoke-all','{}')]:
            self.assertEqual(self.request(endpoint,'POST',{'Content-Type':'application/json'},body)[0],403)
            self.assertEqual(self.request(endpoint,'POST',headers,body)[0],200)
        self.assertFalse(self.engine.network_enabled)
        self.engine.set_network(True)
        pending=self.engine.authorize({'method':'GET','host':'api.github.com','path':'/repos/acme/demo','headers':[]})
        endpoint='/api/request/'+pending['request_id']
        self.assertEqual(self.request(endpoint)[0],401)
        status,body=self.request(endpoint,headers=headers)
        self.assertEqual(status,200);self.assertEqual(json.loads(body)['id'],pending['request_id'])
        self.assertNotIn('fingerprint',json.loads(body))
    def test_pull_request_preview_is_host_only_and_not_persisted(self):
        import base64
        marker='private-pr-review-content-123'
        pending=self.engine.authorize({'method':'POST','host':'api.github.com','path':'/repos/acme/demo/pulls',
            'headers':[['Content-Type','application/json']], 'body_base64':base64.b64encode(json.dumps({'head':'work','base':'main','title':marker,'draft':True}).encode()).decode()})
        body=json.dumps({'request_id':pending['request_id']})
        headers={'Authorization':'Bearer test-session','Content-Type':'application/json','Origin':f'http://127.0.0.1:{self.port}'}
        for route in ['/api/request-review','/api/git-review']:
            self.assertEqual(self.request(route,'POST',{'Content-Type':'application/json'},body)[0],403)
        status,data=self.request('/api/request-review','POST',headers,body)
        self.assertEqual(status,200);self.assertIn(marker.encode(),data)
        self.assertNotIn(marker.encode(),self.request('/api/state',headers=headers)[1])
        for file in Path(self.tmp.name).rglob('*'):
            if file.is_file():self.assertNotIn(marker.encode(),file.read_bytes())
        for key,value in list(self.engine.rest_reviews.items()): self.engine.rest_reviews[key]=(0,value[1])
        self.engine.prune_reviews();self.assertFalse(self.engine.rest_reviews)
        self.assertEqual(self.request('/api/request-review','POST',headers,body)[0],400)
    def test_readiness_needs_enforced_firewall(self):
        with self.assertRaises(ValueError): internal(self.engine,{'action':'ready','firewall':'off'})
        internal(self.engine,{'action':'ready','firewall':'enforced'})
        self.assertTrue((Path(self.tmp.name)/'proxy-ready.json').exists())
    def test_clipboard_host_authorization_and_no_persistence(self):
        text='clipboard-test-private-42\nこんにちは 🌍'
        headers={'Authorization':'Bearer test-session','Content-Type':'application/json','Origin':f'http://127.0.0.1:{self.port}'}
        body=json.dumps({'text':text})
        for overrides in [{'Authorization':'Bearer wrong'}, {'Origin':'https://evil.example'}, {'Host':'evil.example'}]:
            self.assertEqual(self.request('/api/clipboard','POST',{**headers,**overrides},body)[0],403)
        code,response=self.request('/api/clipboard','POST',headers,body)
        self.assertEqual(code,200); transfer_id=json.loads(response)['id']
        state=self.request('/api/state',headers=headers)[1]
        self.assertNotIn(b'clipboard-test-private-42',state)
        for action in ['clipboard.send','clipboard.cancel']:
            with self.assertRaises(ValueError): internal(self.engine,{'action':action,'text':text})
        with self.assertRaises(ValueError): internal(self.engine,{'action':'clipboard.take','text':text})
        self.assertEqual(internal(self.engine,{'action':'clipboard.take'}),{'text':text,'id':transfer_id})
        self.assertEqual(internal(self.engine,{'action':'clipboard.take'}),{'text':None,'id':None})
        self.assertEqual(internal(self.engine,{'action':'clipboard.ack','id':transfer_id}),{'accepted':True})
        code,response=self.request('/api/clipboard/status','POST',headers,json.dumps({'id':transfer_id}))
        self.assertEqual(code,200); self.assertEqual(json.loads(response)['status'],'delivered')
        for path in Path(self.tmp.name).rglob('*'):
            if path.is_file(): self.assertNotIn(b'clipboard-test-private-42',path.read_bytes())
        self.assertEqual(self.request('/api/clipboard','POST',headers,body)[0],200)
        self.assertEqual(self.request('/api/clipboard/cancel','POST',headers,'{}')[0],200)
        self.assertIsNone(self.engine.clipboard.take()['text'])

if __name__=='__main__': unittest.main()
