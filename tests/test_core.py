import base64
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from warden.core import Engine, Operations, Redactor, dumps, github_host, strict_json

TOKEN='ghp_'+'TESTONLYNOTAREALTOKEN'*3
def request(body=None,**overrides):
    result={'method':'POST','host':'api.github.com','path':'/repos/acme/demo/pulls','scheme':'https','port':443,'headers':[['content-type','application/json']],'body_base64':base64.b64encode(json.dumps(body or {'title':'A change','head':'work','base':'main','draft':True}).encode()).decode()}
    result.update(overrides); return result

class EngineTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls): cls.operations=Operations()
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory(); self.time=1000; self.mono=500
        self.engine=Engine(self.temp.name,self.operations,lambda:self.time,lambda:self.mono)
        self.engine.set_token(TOKEN)
    def tearDown(self):
        self.engine.db.close()
        import os
        os.close(self.engine.audit.fd)
        self.temp.cleanup()
    def grant(self,req=None,kind='exact',ttl=60,predicates=None):
        pending=self.engine.authorize(req or request())
        self.assertFalse(pending['allow']); self.assertEqual(pending['status'],428)
        return self.engine.approve(pending['request_id'],kind,ttl,predicates)
    def test_catalog_identifies_individual_operations(self):
        op,params=self.operations.match('PUT','/repos/acme/demo/pulls/12/merge')
        self.assertEqual(op['operation_id'],'pulls/merge'); self.assertEqual(params['pull_number'],'12')
        self.assertGreater(len(self.operations.routes),1000)
    def test_default_deny_never_returns_token(self):
        result=self.engine.authorize(request()); self.assertFalse(result['allow']); self.assertNotIn(TOKEN,dumps(result))
    def test_exact_single_use_and_concurrent_replay(self):
        self.grant()
        with ThreadPoolExecutor(max_workers=12) as pool: results=list(pool.map(lambda _:self.engine.authorize(request()),range(20)))
        allowed=[r for r in results if r['allow']]
        self.assertEqual(len(allowed),1); self.assertEqual(allowed[0]['authorization'],'Bearer '+TOKEN)
    def test_exact_binds_every_byte_and_header(self):
        self.grant()
        for altered in [request({'title':'evil','head':'work','base':'main','draft':True}),request(path='/repos/acme/other/pulls'),request(path='/repos/acme/demo/pulls?x=1'),request(headers=[['content-type','application/json'],['accept','application/vnd.github+json']])]: self.assertFalse(self.engine.authorize(altered)['allow'])
        self.assertTrue(self.engine.authorize(request())['allow'])
    def test_scoped_predicates_and_repetition(self):
        self.grant(kind='scoped',predicates={'/base':'main','/draft':True})
        self.assertTrue(self.engine.authorize(request({'title':'another','base':'main','draft':True}))['allow'])
        self.assertTrue(self.engine.authorize(request())['allow'])
        self.assertFalse(self.engine.authorize(request({'base':'main','draft':1}))['allow'])
        self.assertFalse(self.engine.authorize(request({'base':'release','draft':True}))['allow'])
        self.assertFalse(self.engine.authorize(request({'base':'main'}))['allow'])
    def test_scoped_does_not_cross_operation_or_repository(self):
        self.grant(kind='scoped')
        self.assertFalse(self.engine.authorize(request(path='/repos/acme/demo/issues'))['allow'])
        self.assertFalse(self.engine.authorize(request(path='/repos/acme/other/pulls'))['allow'])
    def test_expiry_revocation_and_clock_rollback(self):
        grant=self.grant(kind='scoped',ttl=10)
        decision=self.engine.authorize(request()); self.assertTrue(self.engine.active(decision['decision_id']))
        self.time=900; self.mono+=11
        self.assertFalse(self.engine.active(decision['decision_id'])); self.assertFalse(self.engine.authorize(request())['allow'])
        grant=self.grant(kind='scoped',ttl=30); decision=self.engine.authorize(request()); self.engine.revoke(grant['grant_id'])
        self.assertFalse(self.engine.active(decision['decision_id']))
    def test_restart_revokes_all_grants(self):
        self.grant(kind='scoped')
        restarted=Engine(self.temp.name,self.operations)
        restarted.set_token(TOKEN)
        self.assertFalse(restarted.authorize(request())['allow'])
        restarted.db.close()
        import os
        os.close(restarted.audit.fd)
    def test_no_token_in_audit_database_or_ui(self):
        req=request({'title':TOKEN,'password':'supersecret','nested':{'api_key':'hidden'}})
        self.grant(req); self.engine.authorize(req)
        audit=(Path(self.temp.name)/'audit/events.jsonl').read_text()
        self.assertNotIn(TOKEN,audit); self.assertNotIn('supersecret',audit); self.assertNotIn('hidden',audit)
        self.assertNotIn(TOKEN,dumps(self.engine.snapshot()))
        rows=dumps([dict(r) for r in self.engine.db.execute('SELECT * FROM requests')]); self.assertNotIn(TOKEN,rows)
        self.assertTrue(self.engine.snapshot()['token_configured'])
    def test_audit_chain_verifies(self):
        self.grant(); self.engine.authorize(request())
        previous='0'*64
        for line in (Path(self.temp.name)/'audit/events.jsonl').read_text().splitlines():
            event=json.loads(line); digest=event.pop('event_hash')
            self.assertEqual(event['previous_hash'],previous); self.assertEqual(hashlib.sha256(dumps(event).encode()).hexdigest(),digest); previous=digest
    def test_audit_failure_never_allows(self):
        self.grant()
        with patch.object(self.engine.audit,'emit',side_effect=OSError('disk full')):
            with self.assertRaises(OSError): self.engine.authorize(request())
    def test_missing_token_fails_closed(self):
        self.grant(); self.engine.token=None
        result=self.engine.authorize(request()); self.assertFalse(result['allow']); self.assertEqual(result['status'],503)
    def test_oauth_token_requires_approval_and_stays_out_of_storage(self):
        token='gho_'+'TESTONLYNOTAREALTOKEN'*3
        self.engine.set_token(token)
        pending=self.engine.authorize(request())
        self.assertEqual(pending['status'],428)
        self.assertNotIn(token,dumps(pending))
        self.engine.approve(pending['request_id'],'exact',60)
        allowed=self.engine.authorize(request())
        self.assertEqual(allowed['authorization'],'Bearer '+token)
        self.assertNotIn(token,dumps(self.engine.snapshot()))
        self.assertEqual(self.engine.redactor.text(token),'[REDACTED]')
        for path in Path(self.temp.name).rglob('*'):
            if path.is_file(): self.assertNotIn(token.encode(),path.read_bytes())
        with self.assertRaises(ValueError): self.engine.set_token('ghr_'+'a'*40)
        with self.assertRaises(ValueError): self.engine.set_token(token+'\r\nInjected: yes')

    def test_unsupported_channels_cannot_be_approved(self):
        for altered in [request(host='github.com'),request(host='evil.example'),request(path='/graphql'),request(path='/unknown-new-operation'),request(scheme='http',port=80),request(port=8443)]:
            self.assertEqual(self.engine.authorize(altered)['status'],403)
    def test_ambiguous_paths_headers_and_json_rejected(self):
        bad=[request(path='//api.github.com/repos/acme/demo/pulls'),request(path='/repos/acme/%2e%2e/pulls'),request(path='/repos/acme/demo/pulls#x'),request(path='/repos/acme/demo/pulls?access_token=secret'),request(path='/repos/acme/demo/pulls?a=1&a=2'),request(headers=[['Authorization','Bearer fake']]),request(headers=[['x','a'],['X','b']]),request(body_base64=base64.b64encode(b'{"a":1,"a":2}').decode()),request(body_base64=base64.b64encode(b'{"x":NaN}').decode()),request(body_base64='%%%')]
        for altered in bad:
            with self.subTest(altered=altered): self.assertEqual(self.engine.authorize(altered)['status'],403)
    def test_policy_deny_and_policy_change_revokes(self):
        self.grant(kind='scoped'); decision=self.engine.authorize(request())
        policy=dict(self.engine.policy); policy['deny_operations']=['pulls/create']; self.engine.save_policy(policy)
        self.assertFalse(self.engine.active(decision['decision_id'])); self.assertEqual(self.engine.authorize(request())['status'],403)
    def test_invalid_grant_cannot_be_created(self):
        pending=self.engine.authorize(request())
        for ttl in [-1,0,3601,True,'60']:
            with self.assertRaises(ValueError): self.engine.approve(pending['request_id'],'exact',ttl)
    def test_github_suffix_boundary(self):
        self.assertTrue(github_host('api.github.com')); self.assertTrue(github_host('raw.githubusercontent.com'))
        self.assertFalse(github_host('github.com.evil.example')); self.assertFalse(github_host('notgithub.com'))
    def test_binary_audit_omitted_and_headers_redacted(self):
        redactor=Redactor()
        self.assertEqual(redactor.body(b'\x00secret')['capture'],'omitted_policy')
        self.assertEqual(redactor.headers([['Authorization','Bearer secret'],['x-arbitrary','secret']]),[['Authorization','[REDACTED]'],['x-arbitrary','[REDACTED]']])

if __name__=='__main__': unittest.main()
