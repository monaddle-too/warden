"""Figma credential isolation, grant boundaries, and OAuth callback security."""
import base64
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit

from warden.core import Engine, dumps
from warden.figma import Connection, SCOPES, protected_host
from warden.server import internal

TOKEN = 'figma_test_access_' + 'x' * 32
REFRESH = 'figma_test_refresh_' + 'y' * 32
CLIENT_SECRET = 'figma_test_client_' + 'z' * 32
GITHUB = 'ghp_' + 'a' * 40


def request(path='/v1/files/TestFile', **fields):
    return {'host':'api.figma.com', 'scheme':'https', 'port':443,
            'method':'GET', 'path':path, 'headers':[], **fields}


class FigmaTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.engine = Engine(self.tmp.name)
        self.engine.set_token(GITHUB)
        self.engine.configure_figma('test-client', CLIENT_SECRET, 'http://127.0.0.1:18765/oauth/figma/callback')
        self.responses = []
        self.engine.figma.transport = self.exchange
        self.connect()

    def exchange(self, client_id, secret, endpoint, fields):
        self.responses.append((client_id, secret, endpoint, fields))
        return {'access_token':TOKEN, 'refresh_token':REFRESH, 'expires_in':3600,
                'user_id_string':'12345', 'token_type':'bearer'}

    def connect(self):
        url = self.engine.connect_figma()['authorization_url']
        params = parse_qs(urlsplit(url).query)
        self.engine.complete_figma(params['state'][0], 'authorization-code')
        return params

    def tearDown(self):
        self.engine.db.close()
        os.close(self.engine.audit.fd)
        self.tmp.cleanup()

    def grant(self, req=None, kind='scoped'):
        req = req or request()
        pending = self.engine.authorize(req)
        self.assertEqual(pending['status'], 428, pending)
        return self.engine.approve(pending['request_id'], kind, 60)

    def test_pkce_and_scope_connection(self):
        params = self.connect()
        self.assertEqual(set(params['scope'][0].split()), set(SCOPES))
        self.assertEqual(params['code_challenge_method'], ['S256'])
        fields = self.responses[-1][3]
        import hashlib
        expected = base64.urlsafe_b64encode(hashlib.sha256(fields['code_verifier'].encode()).digest()).decode().rstrip('=')
        self.assertEqual(params['code_challenge'][0], expected)
        self.assertEqual(fields['redirect_uri'], 'http://127.0.0.1:18765/oauth/figma/callback')

    def test_pending_request_never_contains_token(self):
        pending = self.engine.authorize(request())
        self.assertEqual(pending['status'], 428)
        self.assertNotIn('authorization', pending)

    def test_figma_gets_only_figma_credential(self):
        self.grant()
        decision = self.engine.authorize(request())
        self.assertEqual(decision['authorization'], 'Bearer ' + TOKEN)
        self.assertNotIn(GITHUB, dumps(decision))
        github = {'host':'api.github.com','method':'GET','path':'/user','headers':[]}
        self.grant(github)
        self.assertEqual(self.engine.authorize(github)['authorization'], 'Bearer ' + GITHUB)

    def test_file_query_and_operation_boundaries(self):
        self.grant()
        for req in (request('/v1/files/OtherFile'), request('/v1/files/TestFile?depth=1'),
                    request('/v1/files/TestFile/comments'), request('/v1/me')):
            self.assertEqual(self.engine.authorize(req)['status'], 428)
        self.assertTrue(self.engine.authorize(request())['allow'])

    def test_exact_grant_is_single_use(self):
        self.grant(kind='exact')
        self.assertTrue(self.engine.authorize(request())['allow'])
        self.assertEqual(self.engine.authorize(request())['status'], 428)

    def test_writes_unknown_hosts_and_oauth_are_not_approvable(self):
        for req in (request(method='POST'), request('/v1/files/TestFile/comments', method='POST'),
                    request('/v1/oauth/token'), request('/v1/teams/team/projects'), request(host='www.figma.com'),
                    request(host='api.figma.com.evil.example'), request(scheme='http',port=80),
                    request(port=8443), request('/v1/files/TestFile/images'), request('/v1/files/TestFile/../OtherFile'),
                    request('/v1/files/TestFile%2Fcomments'), request('/v1/files/TestFile?plugin_data=shared'),
                    request('/v1/files/TestFile?access_token=evil'), request('/v1/files/TestFile?depth=1&depth=2'),
                    request(headers=[['X-Figma-Token','guest']]), request(headers=[['Authorization','Bearer guest']]),
                    request(body_base64=base64.b64encode(b'{}').decode())):
            with self.subTest(req=req):
                self.assertEqual(self.engine.authorize(req)['status'], 403)

    def test_valid_read_operations(self):
        for path in ('/v1/me', '/v1/files/TestFile', '/v1/files/TestFile/meta',
                     '/v1/files/TestFile/comments?as_md=true', '/v1/files/TestFile?depth=1',
                     '/v1/files/TestFile/nodes?ids=0%3A1,2%3A3'):
            self.assertEqual(self.engine.authorize(request(path))['status'], 428)

    def test_invalid_node_queries(self):
        for path in ('/v1/files/TestFile/nodes', '/v1/files/TestFile/nodes?ids=bad',
                     '/v1/files/TestFile?depth=-1','/v1/me?ids=0%3A1'):
            self.assertEqual(self.engine.authorize(request(path))['status'], 403)

    def test_file_allowlist_does_not_reuse_repository_policy(self):
        self.engine.save_policy({**self.engine.policy, 'allowed_repositories':[], 'allowed_figma_files':['TestFile']})
        self.grant()
        self.assertTrue(self.engine.authorize(request())['allow'])
        self.assertEqual(self.engine.authorize(request('/v1/files/OtherFile'))['status'],403)
        self.assertEqual(self.engine.authorize({'host':'api.github.com','method':'GET','path':'/repos/acme/demo','headers':[]})['status'],403)

    def test_disconnect_and_reconnect_revoke_existing_grants(self):
        self.grant()
        result = self.engine.authorize(request())
        self.engine.disconnect_figma()
        self.assertFalse(self.engine.active(result['decision_id']))
        self.connect()
        self.assertEqual(self.engine.authorize(request())['status'],428)
        self.grant()
        result = self.engine.authorize(request())
        self.connect()
        self.assertFalse(self.engine.active(result['decision_id']))

    def test_missing_credential_does_not_consume_grant_or_fallback_to_github(self):
        grant = self.grant(kind='exact')
        self.engine.figma.disconnect()
        result = self.engine.authorize(request())
        self.assertEqual(result['status'],503)
        self.assertNotIn('authorization',result)
        row = self.engine.db.execute('SELECT remaining FROM grants WHERE id=?',(grant['grant_id'],)).fetchone()
        self.assertEqual(row['remaining'],1)

    def test_refresh_uses_correct_endpoint_and_never_discloses_refresh_token(self):
        self.grant()
        self.engine.figma.expires = 0
        result = self.engine.authorize(request())
        self.assertTrue(result['allow'])
        self.assertEqual(self.responses[-1][2],'/v1/oauth/refresh')
        self.assertEqual(self.responses[-1][3],{'refresh_token':REFRESH})
        self.assertNotIn(REFRESH,dumps(result))

    def test_refresh_failure_is_closed(self):
        self.grant()
        self.engine.figma.expires = 0
        with patch.object(self.engine.figma,'transport',side_effect=OSError('private error')):
            result = self.engine.authorize(request())
        self.assertEqual(result['status'],503)
        self.assertNotIn('private error',dumps(result))

    def test_bad_callback_cannot_revoke_or_replace_connection(self):
        self.grant()
        decision = self.engine.authorize(request())
        self.engine.connect_figma()
        calls = len(self.responses)
        with self.assertRaises(ValueError): self.engine.complete_figma('bad-state','bad-code')
        self.assertEqual(len(self.responses),calls)
        self.assertTrue(self.engine.active(decision['decision_id']))
        self.assertEqual(self.engine.figma.access_token,TOKEN)

    def test_callback_state_replay_expiry_and_latest_attempt(self):
        old = parse_qs(urlsplit(self.engine.connect_figma()['authorization_url']).query)['state'][0]
        new = parse_qs(urlsplit(self.engine.connect_figma()['authorization_url']).query)['state'][0]
        with self.assertRaises(ValueError): self.engine.complete_figma(old,'code')
        self.engine.complete_figma(new,'code')
        with self.assertRaises(ValueError): self.engine.complete_figma(new,'code')
        current = parse_qs(urlsplit(self.engine.connect_figma()['authorization_url']).query)['state'][0]
        pending = self.engine.figma.pending
        self.engine.figma.pending = (pending[0],pending[1],0)
        with self.assertRaises(ValueError): self.engine.complete_figma(current,'code')

    def test_connection_is_memory_only_and_snapshots_are_secret_free(self):
        state = dumps(self.engine.snapshot())
        for value in (TOKEN,REFRESH,CLIENT_SECRET,GITHUB):
            self.assertNotIn(value,state)
            for path in Path(self.tmp.name).rglob('*'):
                if path.is_file(): self.assertNotIn(value.encode(),path.read_bytes())
        other = Engine(self.tmp.name)
        try: self.assertFalse(other.figma.snapshot()['connected'])
        finally: other.db.close(); os.close(other.audit.fd)

    def test_proxy_cannot_configure_connect_or_disconnect(self):
        for action in ('figma/configure','figma/connect','figma/disconnect','figma/complete'):
            with self.assertRaises(ValueError): internal(self.engine,{'action':action})

    def test_figma_host_boundaries(self):
        self.assertTrue(protected_host('api.figma.com'))
        self.assertTrue(protected_host('mcp.figma.com'))
        self.assertFalse(protected_host('api.figma.com.evil.example'))

    def test_audit_failure_cannot_return_credentials(self):
        self.grant()
        with patch.object(self.engine.audit,'emit',side_effect=OSError('disk full')):
            with self.assertRaises(OSError): self.engine.authorize(request())
