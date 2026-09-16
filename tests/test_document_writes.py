import base64
import json
import tempfile
import unittest
from unittest.mock import Mock
from warden.sharing import Sharing

class DocumentWriteTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory(); self.now=1000
  self.google=Mock(); self.google.file.side_effect=lambda id:{'id':id,'title':id}; self.google.create.return_value={'id':'new-doc','title':'Plan'}
  self.s=Sharing(self.tmp.name,self.google,lambda:self.now)
 def tearDown(self):self.s.db.close();self.tmp.cleanup()
 def request(self,access='write',call='1'):
  return self.s.dispatch('request',{'chatID':'chat','sandboxID':'sbx','callID':call,'reason':'Plan','title':'Plan','access':access})
 def grant(self,access='write',call='1'):
  r=self.request(access,call);return self.s.dispatch('resolve',{'id':r['request_id'],'allow':True,'documents':['doc'],'duration':900})
 def write(self,doc='doc',chat='chat',edits=None,path=None):
  body=json.dumps({'requests':edits if edits is not None else [{'insertText':{'location':{'index':1},'text':'Hello'}}]}).encode()
  return self.s.authorize(chat,'sbx',{'scheme':'https','port':443,'host':'docs.googleapis.com','method':'POST','path':path or '/v1/documents/'+doc+':batchUpdate','body_base64':base64.b64encode(body).decode()})
 def test_write_grant_is_scoped_and_revocable(self):
  g=self.grant();self.write()
  self.write(chat='other')  # Write grants belong to the environment, not the requesting chat.
  for kw in ({'doc':'other'},{'path':'/v1/documents'},{'path':'/v1/documents/doc:batchUpdate?fields=*'},{'edits':[{'insertInlineImage':{'uri':'https://evil.test'}}]}):
   with self.assertRaises(ValueError):self.write(**kw)
  self.now=1900
  with self.assertRaises(ValueError):self.write()
  self.now=1001;self.s.dispatch('revoke',{'id':g['request_id']})
  with self.assertRaises(ValueError):self.write()
 def test_read_grant_never_authorizes_write(self):
  self.grant('read')
  with self.assertRaises(ValueError):self.write()
 def test_creation_requires_approval_and_is_idempotent(self):
  r=self.request('create');self.google.create.assert_not_called()
  data={'id':r['request_id'],'allow':True,'duration':900}
  g=self.s.dispatch('resolve',data);self.assertEqual(g['documents'][0]['id'],'new-doc');self.write('new-doc')
  self.s.dispatch('resolve',data);self.google.create.assert_called_once_with('Plan')
  with self.assertRaises(ValueError):self.write('other')
 def test_uncertain_creation_is_never_retried(self):
  self.google.create.side_effect=TimeoutError()
  r=self.request('create');data={'id':r['request_id'],'allow':True,'duration':900}
  self.assertEqual(self.s.dispatch('resolve',data)['status'],'failed');self.s.dispatch('resolve',data)
  self.google.create.assert_called_once()
 def test_creation_restart_and_denial(self):
  r=self.request('create')
  with self.s.db:self.s.db.execute("UPDATE requests SET status='creating' WHERE id=?",(r['request_id'],))
  self.s.db.close();self.s=Sharing(self.tmp.name,self.google,lambda:self.now)
  self.assertEqual(self.s.dispatch('resolve',{'id':r['request_id'],'allow':True})['status'],'failed')
  r=self.request('create','2');self.s.dispatch('resolve',{'id':r['request_id'],'allow':False});self.google.create.assert_not_called()
 def test_owner_setup_does_not_launch_agent_and_can_be_read_only(self):
  r=self.s.dispatch('select',{'chatID':'codex','sandboxID':'sbx','access':'read','documents':['doc'],'duration':86400})
  self.assertEqual(r['status'],'granted');self.assertEqual(self.s.dispatch('undelivered',{})['requests'],[])
  with self.assertRaises(ValueError):self.write(chat='codex')

class GoogleScopeTests(unittest.TestCase):
 def test_public_chat_callback_is_https_and_exact(self):
  from warden.sharing import GoogleConnection
  with tempfile.TemporaryDirectory() as root:
   g=GoogleConnection(root,None)
   try:
    g.configure('test.apps.googleusercontent.com','test-secret','https://warden.monaddle.com/oauth/google_docs/callback')
    self.assertEqual(g.redirect_uri,'https://warden.monaddle.com/oauth/google_docs/callback')
    for uri in ['http://warden.monaddle.com/oauth/google_docs/callback','https://user@warden.monaddle.com/oauth/google_docs/callback','https://warden.monaddle.com/wrong','https://warden.monaddle.com/oauth/google_docs/callback?x=1']:
     with self.assertRaises(ValueError):g.configure('test.apps.googleusercontent.com','test-secret',uri)
   finally:g.db.close()
 def test_legacy_read_connection_survives_upgrade(self):
  import os
  import sqlite3
  from pathlib import Path
  from warden.sharing import GoogleConnection
  with tempfile.TemporaryDirectory() as root:
   p=Path(root);config=p/'config.json';config.write_text(json.dumps({'client_id':'client.apps.googleusercontent.com','client_secret':'synthetic-secret','redirect_uri':'http://127.0.0.1:18781/oauth/google_docs/callback'}));config.chmod(0o600)
   with sqlite3.connect(p/'google.sqlite') as db:
    db.execute('CREATE TABLE credentials (id INTEGER PRIMARY KEY, data TEXT)')
    db.execute('INSERT INTO credentials VALUES (1,?)',(json.dumps({'client_id':'client.apps.googleusercontent.com','access_token':'synthetic-access-token','refresh_token':'synthetic-refresh-token','expires':9999999999}),))
   c=GoogleConnection(p,config)
   self.assertTrue(c.access_token);self.assertFalse(c.can_write())
   with self.assertRaises(ValueError):c.create('Plan')
   scopes='https://www.googleapis.com/auth/documents https://www.googleapis.com/auth/drive.metadata.readonly'
   c._install({'access_token':'synthetic-new-access-token','refresh_token':'synthetic-new-refresh-token','expires_in':3600,'scope':scopes},initial=True)
   self.assertTrue(c.can_write());c.db.close()
   c=GoogleConnection(p,config);self.assertTrue(c.can_write());c.db.close()
