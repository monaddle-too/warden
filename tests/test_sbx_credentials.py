import base64
import json
import os
from pathlib import Path
import tempfile
import unittest

from warden.sbx_credentials import CodexCredentials


def jwt(exp):
    payload = base64.urlsafe_b64encode(json.dumps({'exp': exp}).encode()).decode().rstrip('=')
    return 'fixture.' + payload + '.signature'


class CodexCredentialTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / 'auth.json'
        self.write(jwt(500))
        self.provider = CodexCredentials(self.path, clock=lambda: 100)

    def write(self, token):
        self.path.write_text(json.dumps({'auth_mode': 'chatgpt', 'tokens': {
            'access_token': token, 'account_id': 'fixture-account',
            'refresh_token': 'never-export-this-refresh-token', 'id_token': 'never-export-id-token'}}))
        self.path.chmod(0o600)

    def test_fixed_route_and_only_required_headers(self):
        self.assertTrue(self.provider.available())
        self.assertEqual(self.provider.route('/v1/responses'), {'host': 'chatgpt.com', 'path': '/backend-api/codex/responses'})
        self.assertEqual(self.provider.headers(), {'Authorization': 'Bearer ' + jwt(500), 'ChatGPT-Account-ID': 'fixture-account'})
        for path in ['/v1/chat/completions', '/v1/responses?target=other', '//evil/responses']:
            with self.assertRaises(ValueError):
                self.provider.route(path)

    def test_refresh_is_read_without_modifying_source(self):
        self.write(jwt(900))
        before = self.path.read_bytes()
        self.assertEqual(self.provider.headers()['Authorization'], 'Bearer ' + jwt(900))
        self.assertEqual(before, self.path.read_bytes())

    def test_expired_malformed_public_and_symlink_fail_closed(self):
        for token in [jwt(110), 'not-a-token', 'secret\r\nInjected:yes']:
            self.write(token)
            self.assertFalse(self.provider.available())
        self.write(jwt(500))
        self.path.chmod(0o644)
        self.assertFalse(self.provider.available())
        target = self.path.with_name('actual.json')
        self.path.rename(target)
        self.path.symlink_to(target)
        self.assertFalse(self.provider.available())

    def test_account_header_cannot_be_injected(self):
        data = json.loads(self.path.read_text())
        data['tokens']['account_id'] = 'fixture\r\nInjected: yes'
        self.path.write_text(json.dumps(data))
        self.assertFalse(self.provider.available())

    def test_non_object_documents_fail_closed(self):
        self.path.write_text('[]')
        self.assertFalse(self.provider.available())
        self.write('fixture.W10.signature')
        self.assertFalse(self.provider.available())

class ClaudeCredentialTests(unittest.TestCase):
    def test_private_expiring_access_token_only(self):
        from warden.sbx_credentials import ClaudeCredentials
        with tempfile.TemporaryDirectory() as root:
            path = Path(root)/'claude.json'
            document = {'claudeAiOauth': {'accessToken': 'fixture-access-token-long', 'refreshToken':'never-export-refresh', 'expiresAt':500000}}
            path.write_text(json.dumps(document));path.chmod(0o600)
            source=ClaudeCredentials(path,clock=lambda:100)
            self.assertTrue(source.available())
            self.assertEqual(source.headers(),{'Authorization':'Bearer fixture-access-token-long'})
            self.assertEqual(source.route('/v1/messages')['host'],'api.anthropic.com')
            with self.assertRaises(ValueError):source.route('/v1/responses')
            for data in [[], {'claudeAiOauth':[]}, {'claudeAiOauth':{'accessToken':'fixture-access-token-long','expiresAt':100001}}]:
                path.write_text(json.dumps(data));self.assertFalse(source.available())
            path.write_text(json.dumps(document));path.chmod(0o644);self.assertFalse(source.available())
