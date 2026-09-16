import json
import unittest
from urllib.parse import parse_qs, urlsplit, urlencode
import test_server


class FigmaServerTests(unittest.TestCase):
    setUp = test_server.ServerTests.setUp
    tearDown = test_server.ServerTests.tearDown
    request = test_server.ServerTests.request

    def headers(self):
        return {'Authorization':'Bearer test-session','Content-Type':'application/json',
                'Origin':f'http://127.0.0.1:{self.port}'}

    def test_configuration_requires_owner_authentication_and_same_origin(self):
        body=json.dumps({'client_id':'test','client_secret':'test-secret-private'})
        path='/api/figma/configure'
        for headers in ({'Content-Type':'application/json'}, {**self.headers(),'Origin':'https://evil.example'}):
            self.assertEqual(self.request(path,'POST',headers,body)[0],403)
        self.assertEqual(self.request(path,'POST',self.headers(),body)[0],200)
        self.assertEqual(self.engine.figma.redirect_uri,f'http://127.0.0.1:{self.port}/oauth/figma/callback')
        for path in ('/api/figma/connect','/api/figma/disconnect'):
            self.assertEqual(self.request(path,'POST',{'Content-Type':'application/json'},'{}')[0],403)

    def test_oauth_callback_uses_single_use_state_without_exposing_tokens(self):
        self.engine.configure_figma('test','test-secret-private',f'http://127.0.0.1:{self.port}/oauth/figma/callback')
        token='figma_private_access_token'
        self.engine.figma.transport=lambda *_: {'access_token':token,'refresh_token':'figma_private_refresh_token','expires_in':3600,'user_id_string':'12345'}
        status,body=self.request('/api/figma/connect','POST',self.headers(),'{}')
        self.assertEqual(status,200)
        query=parse_qs(urlsplit(json.loads(body)['authorization_url']).query)
        callback='/oauth/figma/callback?'+urlencode({'state':query['state'][0],'code':'a-code'})
        self.assertEqual(self.request(callback+'&code=duplicate')[0],400)
        self.assertEqual(self.request(callback.replace(query['state'][0],'wrong'))[0],400)
        status,body=self.request(callback)
        self.assertEqual(status,200)
        self.assertNotIn(token.encode(),body)
        self.assertTrue(self.engine.figma.snapshot()['connected'])
        self.assertEqual(self.request(callback)[0],400)
        self.assertEqual(self.request('/api/figma/disconnect','POST',self.headers(),'{}')[0],200)
        self.assertFalse(self.engine.figma.snapshot()['connected'])
