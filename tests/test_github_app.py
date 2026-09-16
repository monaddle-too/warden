import json
import os
from pathlib import Path
import sqlite3
import tempfile
import time
import unittest
from unittest.mock import patch
from datetime import datetime,timezone
from types import SimpleNamespace
from warden.core import Engine,Redactor
from warden.github_app import StoreBroker,Credentials,permissions,from_config

TOKEN='ghs_'+('a'*520)
REPO='owner/repo'

def result(repo=REPO, perms=None):
    return {'token':TOKEN,'expires_at':datetime.fromtimestamp(time.time()+3500,timezone.utc).isoformat(),
            'permissions':perms or {'metadata':'read'},'repository':repo,'app_id':123,'installation_id':456}

class AppTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.engine=Engine(self.tmp.name)
        self.engine.github_app=Credentials(['/trusted/broker'],'owner',123,self.engine.redactor,self.engine.now)
        self.calls=[]
        def run(command,**kw):
            self.calls.append(json.loads(kw['input']))
            return SimpleNamespace(stdout=json.dumps(result(perms=permissions(self.calls[-1]['operation']))).encode())
        self.mock=patch('warden.github_app.subprocess.run',side_effect=run);self.mock.start()
    def tearDown(self):
        self.mock.stop();self.engine.db.close();os.close(self.engine.audit.fd);self.tmp.cleanup()
    def req(self,path='/repos/owner/repo',method='GET'):
        return {'host':'api.github.com','path':path,'method':method,'headers':[]}
    def grant(self,req=None):
        req=req or self.req();r=self.engine.authorize(req);self.assertEqual(r['status'],428)
        return self.engine.approve(r['request_id'],'exact',60)
    def test_no_token_needed_or_minted_before_approval(self):
        self.grant();self.assertEqual(self.calls,[])
        r=self.engine.authorize(self.req());self.assertEqual(r['authorization'],'Bearer '+TOKEN)
        self.assertEqual(self.calls,[{'repository':REPO,'operation':'repos/get'}])
        self.assertEqual(self.engine.authorize(self.req())['status'],428)
    def test_host_owner_and_operation_boundary(self):
        for p in ('/repos/other/repo','/user','/repos/owner/repo/actions/runs'):
            self.assertEqual(self.engine.authorize(self.req(p))['status'],403)
        self.assertEqual(self.calls,[])
    def test_other_repository_requires_approval(self):
        self.grant();self.assertEqual(self.engine.authorize(self.req('/repos/owner/another'))['status'],428)
        self.assertEqual(self.calls,[])
    def test_source_failure_preserves_grant_and_never_falls_back(self):
        grant=self.grant();self.engine.token='ghp_'+'z'*40
        with patch('warden.github_app.subprocess.run',side_effect=OSError('secret diagnostic')):
            r=self.engine.authorize(self.req())
        self.assertEqual(r['status'],503);self.assertNotIn('authorization',r)
        self.assertEqual(self.engine.db.execute('SELECT remaining FROM grants WHERE id=?',(grant['grant_id'],)).fetchone()[0],1)
    def test_manual_token_rejected_in_app_mode(self):
        with self.assertRaises(ValueError):self.engine.set_token('ghp_'+'z'*40)
    def test_malformed_or_overbroad_broker_response(self):
        self.grant()
        for changed in ({'repository':'owner/other'},{'permissions':{'contents':'write'}},{'app_id':999},
                        {'token':'ghp_'+'x'*30},{'expires_at':'2000-01-01T00:00:00Z'}):
            with patch('warden.github_app.subprocess.run',return_value=SimpleNamespace(stdout=json.dumps({**result(),**changed}).encode())):
                self.assertEqual(self.engine.authorize(self.req())['status'],503)
    def test_google_and_figma_credentials_stay_separate(self):
        self.engine.figma.access_token='figma_test';self.engine.figma.expires=time.time()+3600
        r={'host':'api.figma.com','method':'GET','path':'/v1/me','headers':[]}
        self.grant(r);self.assertEqual(self.engine.authorize(r)['authorization'],'Bearer figma_test');self.assertEqual(self.calls,[])
    def test_snapshot_and_store_never_contain_token(self):
        self.grant();self.engine.authorize(self.req())
        self.assertNotIn(TOKEN,json.dumps(self.engine.snapshot()))
        for p in Path(self.tmp.name).rglob('*'):
            if p.is_file():self.assertNotIn(TOKEN.encode(),p.read_bytes())
    def test_write_permissions_are_narrow_and_separate(self):
        req=self.req('/repos/owner/repo/git/blobs','POST')
        import base64
        req['headers']=[['Content-Type','application/json']]
        req['body_base64']=base64.b64encode(b'{"content":"test","encoding":"utf-8"}').decode()
        self.grant(req)
        self.assertTrue(self.engine.authorize(req)['allow'])
        self.assertEqual(self.calls[-1]['operation'],'git/create-blob')
        self.assertEqual(permissions('git/create-blob'),{'contents':'write','metadata':'read'})
        self.assertEqual(permissions('pulls/create'),{'pull_requests':'write','metadata':'read'})

    def test_multiple_engines_share_source_without_sharing_grants(self):
        other=Engine(Path(self.tmp.name)/'other')
        try:
            other.github_app=self.engine.github_app
            self.grant()
            self.assertEqual(other.authorize(self.req())['status'],428)
            self.assertEqual(self.calls,[])
            self.assertTrue(self.engine.authorize(self.req())['allow'])
        finally:other.db.close();os.close(other.audit.fd)

    def test_new_approval_rechecks_installation(self):
        for _ in range(2):self.grant();self.engine.authorize(self.req())
        self.assertEqual(len(self.calls),2)
    def test_expired_grant_after_broker_does_not_dispatch(self):
        self.grant()
        now=[time.time()]
        def auth(*_):
            now[0]+=120
            return 'Bearer '+TOKEN
        with patch.object(self.engine,'now',side_effect=lambda:now[0]):
            with patch.object(self.engine.github_app,'authorization',side_effect=auth):
                r=self.engine.authorize(self.req())
        self.assertFalse(r['allow'])

class StoreTests(unittest.TestCase):
    def setUp(self):
        from cryptography.hazmat.primitives.asymmetric import rsa
        from cryptography.hazmat.primitives import serialization
        from cryptography.hazmat.primitives.ciphers.aead import AESGCM
        self.tmp=tempfile.TemporaryDirectory();p=Path(self.tmp.name);self.path=p
        self.key=rsa.generate_private_key(public_exponent=65537,key_size=2048)
        pem=self.key.private_bytes(serialization.Encoding.PEM,serialization.PrivateFormat.PKCS8,serialization.NoEncryption()).decode()
        key=os.urandom(32);(p/'encryption.key').write_bytes(key);(p/'encryption.key').chmod(0o600)
        db=sqlite3.connect(p/'github.sqlite');db.execute('CREATE TABLE secrets(key TEXT PRIMARY KEY,value BLOB)')
        nonce=os.urandom(12);value=json.dumps({'id':123,'slug':'monaddle-workspace','pem':pem}).encode()
        db.execute('INSERT INTO secrets VALUES(?,?)',('app',nonce+AESGCM(key).encrypt(nonce,value,b'app')))
        db.execute('INSERT INTO secrets VALUES(?,?)',('user:never-read',b'invalid encrypted user record'));db.commit();db.close()
        self.calls=[]
        def transport(method,path,token,body=None):
            self.calls.append((method,path,token,body))
            if method=='GET':return {'id':456,'app_id':123,'account':{'login':'owner'},'suspended_at':None}
            return {**result(perms=body['permissions']),'repositories':[{'full_name':REPO}]}
        self.broker=StoreBroker(p,123,'owner',transport)
    def tearDown(self):self.tmp.cleanup()
    def test_existing_app_key_signs_jwt_without_user_tokens(self):
        from cryptography.hazmat.primitives.asymmetric import padding
        from cryptography.hazmat.primitives import hashes
        import base64
        out=self.broker.issue({'repository':REPO,'operation':'git/read'})
        self.assertEqual(out['token'],TOKEN)
        self.assertEqual(self.calls[1][3],{'repositories':['repo'],'permissions':{'contents':'read','metadata':'read'}})
        jwt=self.calls[0][2];data,sig=jwt.rsplit('.',1)
        self.key.public_key().verify(base64.urlsafe_b64decode(sig+'='*(-len(sig)%4)),data.encode(),padding.PKCS1v15(),hashes.SHA256())
    def test_unauthorized_owner_rejected_before_key_access(self):
        with patch.object(self.broker,'app',side_effect=AssertionError('must not read')):
            with self.assertRaises(ValueError):self.broker.issue({'repository':'other/repo','operation':'repos/get'})
    def test_suspended_or_wrong_installation_fails(self):
        for change in ({'app_id':999},{'suspended_at':'now'},{'account':{'login':'other'}}):
            self.broker.transport=lambda *_:dict({'id':456,'app_id':123,'account':{'login':'owner'}},**change)
            with self.assertRaises(ValueError):self.broker.issue({'repository':REPO,'operation':'repos/get'})
    def test_private_config_required(self):
        p=self.path/'source.json';p.write_text(json.dumps({'command':['/broker'],'owner':'owner','app_id':123}));p.chmod(0o644)
        with self.assertRaises(ValueError):from_config(p,Redactor())
        p.chmod(0o600);self.assertEqual(from_config(p,Redactor()).owner,'owner')
    def test_unknown_operations_fail_closed(self):
        for op in ('admin/delete','issues/create','users/get-authenticated'):
            with self.assertRaises(ValueError):permissions(op)

    def test_discovery_uses_metadata_only_and_filters_owned_repositories(self):
        calls=[]
        def transport(method,path,token,body=None):
            calls.append((method,path,body))
            if path.startswith('/users/'):
                return {'id':456,'app_id':123,'account':{'login':'owner','type':'User'}}
            if method=='POST': return {'token':TOKEN,'permissions':{'metadata':'read'}}
            return {'repositories':[{'id':1,'full_name':'owner/private','private':True,'owner':{'login':'owner'}},
                {'id':2,'full_name':'other/private','owner':{'login':'other'}}]}
        self.broker.transport=transport
        out=self.broker.repositories(2)
        self.assertEqual(out['repositories'],[{'id':1,'full_name':'owner/private','private':True}])
        self.assertEqual(calls[1][2],{'permissions':{'metadata':'read'}})
        self.assertEqual(calls[2][1],'/installation/repositories?per_page=100&page=2')
        self.assertNotIn(TOKEN,json.dumps(out))
        with self.assertRaises(ValueError):self.broker.repositories(0)
