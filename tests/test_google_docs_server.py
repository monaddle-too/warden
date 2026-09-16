import json
import unittest
from urllib.parse import parse_qs, urlsplit, urlencode
import test_server


class GoogleDocsServerTests(unittest.TestCase):
    setUp = test_server.ServerTests.setUp
    tearDown = test_server.ServerTests.tearDown
    request = test_server.ServerTests.request

    def headers(self):
        return {'Authorization':'Bearer test-session','Content-Type':'application/json',
                'Origin':f'http://127.0.0.1:{self.port}'}

    def test_configuration_requires_owner_authentication_and_same_origin(self):
        body=json.dumps({'client_id':'test.apps.googleusercontent.com','client_secret':'test-secret-private'})
        path='/api/google_docs/configure'
        for headers in ({'Content-Type':'application/json'}, {**self.headers(),'Origin':'https://evil.example'}):
            self.assertEqual(self.request(path,'POST',headers,body)[0],403)
        self.assertEqual(self.request(path,'POST',self.headers(),body)[0],200)
        self.assertEqual(self.engine.google_docs.redirect_uri,f'http://127.0.0.1:{self.port}/oauth/google_docs/callback')
        for path in ('/api/google_docs/connect','/api/google_docs/disconnect'):
            self.assertEqual(self.request(path,'POST',{'Content-Type':'application/json'},'{}')[0],403)

    def test_oauth_callback_uses_single_use_state_without_exposing_tokens(self):
        self.engine.configure_google_docs('test.apps.googleusercontent.com','test-secret-private',f'http://127.0.0.1:{self.port}/oauth/google_docs/callback')
        token='google_docs_private_access_token'
        self.engine.google_docs.transport=lambda *_: {'access_token':token,'refresh_token':'google_docs_private_refresh_token','expires_in':3600,'scope':'https://www.googleapis.com/auth/documents.readonly'}
        status,body=self.request('/api/google_docs/connect','POST',self.headers(),'{}')
        self.assertEqual(status,200)
        query=parse_qs(urlsplit(json.loads(body)['authorization_url']).query)
        callback='/oauth/google_docs/callback?'+urlencode({'state':query['state'][0],'code':'a-code','scope':'https://www.googleapis.com/auth/documents.readonly','authuser':'0','prompt':'consent','iss':'https://accounts.google.com'})
        self.assertEqual(self.request(callback.replace('accounts.google.com','evil.example'))[0],400)
        self.assertEqual(self.request(callback+'&code=duplicate')[0],400)
        self.assertEqual(self.request(callback.replace(query['state'][0],'wrong'))[0],400)
        status,body=self.request(callback)
        self.assertEqual(status,200)
        self.assertNotIn(token.encode(),body)
        self.assertTrue(self.engine.google_docs.snapshot()['connected'])
        self.assertEqual(self.request(callback)[0],400)
        self.assertEqual(self.request('/api/google_docs/disconnect','POST',self.headers(),'{}')[0],200)
        self.assertFalse(self.engine.google_docs.snapshot()['connected'])
