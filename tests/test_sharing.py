import tempfile
import unittest
from unittest.mock import Mock
from warden.sharing import Sharing

class SharingTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.now=1000
  self.google=Mock();self.google.file.side_effect=lambda id:{'id':id,'title':id,'url':'https://docs.google.com/document/d/'+id+'/edit','api_url':'https://docs.googleapis.com/v1/documents/'+id}
  self.google.authorization.return_value='Bearer synthetic-google-secret'
  self.s=Sharing(self.tmp.name,self.google,lambda:self.now)
 def tearDown(self):self.s.db.close();self.tmp.cleanup()
 def request(self,**kw):return self.s.dispatch('request',{'chatID':'chat-a','sandboxID':'sbx-a','callID':'tool1','reason':'Read the plan',**kw})
 def grant(self):
  r=self.request();return self.s.dispatch('resolve',{'id':r['request_id'],'allow':True,'documents':['doc-a'],'duration':900})
 def read(self,doc='doc-a',chat='chat-a',sandbox='sbx-a',**kw):
  return self.s.authorize(chat,sandbox,{'method':'GET','scheme':'https','port':443,'host':'docs.googleapis.com','path':'/v1/documents/'+doc,'body_base64':'',**kw})
 def test_durable_request_idempotency_and_completion(self):
  r=self.request();self.assertEqual(self.request()['request_id'],r['request_id']);self.s.db.close();self.s=Sharing(self.tmp.name,self.google,lambda:self.now)
  self.assertEqual(self.s.dispatch('state',{})['requests'][0]['status'],'pending')
  g=self.grant();self.assertEqual(len(self.s.dispatch('undelivered',{})['requests']),1)
  self.s.dispatch('ack',{'id':g['request_id']});self.assertEqual(self.s.dispatch('undelivered',{})['requests'],[])
 def test_scope_expiry_and_revocation(self):
  g=self.grant();self.assertEqual(self.read()[1],'Bearer synthetic-google-secret')
  # Another chat on the same environment shares the grant; another environment does not.
  self.assertEqual(self.read(chat='chat-b')[1],'Bearer synthetic-google-secret')
  self.assertEqual(self.s.dispatch('get',{'id':g['request_id'],'chatID':'chat-b','sandboxID':'sbx-a'})['status'],'granted')
  self.assertTrue(self.s.active(g['request_id'],'chat-b','sbx-a'))
  self.assertFalse(self.s.active(g['request_id'],'chat-a','sbx-b'))
  for kwargs in ({'doc':'doc-b'},{'sandbox':'sbx-b'},{'method':'POST'},{'host':'www.googleapis.com'},{'path':'/v1/documents/doc-a:batchUpdate'},{'path':'/v1/documents/doc-a?fields=*'},{'body_base64':'e30='}):
   with self.assertRaises(ValueError):self.read(**kwargs)
  self.now=1900
  with self.assertRaises(ValueError):self.read()
  self.now=1001;self.s.dispatch('revoke',{'id':g['request_id']})
  with self.assertRaises(ValueError):self.read()
 def test_deny_does_not_grant_and_cannot_be_changed(self):
  r=self.request();self.s.dispatch('resolve',{'id':r['request_id'],'allow':False})
  self.assertEqual(self.grant()['status'],'denied')
  with self.assertRaises(ValueError):self.read()
 def test_owner_selection_is_verified_before_commit(self):
  r=self.request();self.google.file.side_effect=ValueError('no such file')
  with self.assertRaises(ValueError):self.grant()
  self.assertEqual(self.s.dispatch('state',{})['requests'][0]['status'],'pending')
  self.assertEqual(self.s.dispatch('list',{'chatID':'chat-a','sandboxID':'sbx-a'})['grants'],[])
 def test_unsharable_tag_blocks_grants_and_revokes_existing_access(self):
  g=self.grant();self.assertEqual(self.read()[1],'Bearer synthetic-google-secret')
  self.google.files.return_value={'files':[{'id':'doc-a','name':'Plan'},{'id':'doc-b','name':'Notes'}],'nextPageToken':'p2'}
  self.assertEqual([f['blocked'] for f in self.s.dispatch('files',{})['files']],[False,False])
  self.assertEqual(self.s.dispatch('status',{})['github'],{'connected':False,'owner':''})
  r=self.s.dispatch('block',{'id':'doc-a','name':'Plan'})
  self.assertEqual(r['revoked'],[g['request_id']])
  self.assertEqual(self.s.dispatch('state',{})['requests'][0]['status'],'revoked')
  with self.assertRaises(ValueError):self.read()
  self.assertEqual(self.s.dispatch('blocked',{})['documents'],[{'id':'doc-a','name':'Plan','blocked_at':1000}])
  self.assertEqual([f['blocked'] for f in self.s.dispatch('files',{})['files']],[True,False])
  self.assertEqual(self.s.dispatch('files',{})['nextPageToken'],'p2')
  # New grants naming the tagged document are refused and stay pending; other documents still work.
  n=self.s.dispatch('request',{'chatID':'chat-a','sandboxID':'sbx-a','callID':'tool2','reason':'Read it again'})
  with self.assertRaises(ValueError):self.s.dispatch('resolve',{'id':n['request_id'],'allow':True,'documents':['doc-b','doc-a'],'duration':900})
  self.assertEqual(self.s.dispatch('get',{'id':n['request_id'],'chatID':'chat-a','sandboxID':'sbx-a'})['status'],'pending')
  self.assertEqual(self.s.dispatch('resolve',{'id':n['request_id'],'allow':True,'documents':['doc-b'],'duration':900})['status'],'granted')
  self.assertEqual(self.read(doc='doc-b')[1],'Bearer synthetic-google-secret')
  with self.assertRaises(ValueError):self.read()
  # A stale grant that somehow names a tagged document is still refused at dispatch.
  self.s.db.execute("UPDATE requests SET documents=? WHERE id=?",('[{"id":"doc-a","title":"Plan","url":"","api_url":""}]',n['request_id']));self.s.db.commit()
  with self.assertRaises(ValueError):self.read()
  for bad in ({'id':'../x'},{'id':''},{'id':7},{'id':'doc-a','name':'x'*201}):
   with self.assertRaises(ValueError):self.s.dispatch('block',bad)
  self.s.db.close();self.s=Sharing(self.tmp.name,self.google,lambda:self.now)
  self.assertEqual(len(self.s.dispatch('blocked',{})['documents']),1)
  self.s.dispatch('unblock',{'id':'doc-a'})
  self.assertEqual(self.s.dispatch('blocked',{})['documents'],[])
  self.assertEqual([f['blocked'] for f in self.s.dispatch('files',{})['files']],[False,False])
