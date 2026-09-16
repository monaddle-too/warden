import hashlib
import json
import os
from pathlib import Path
import socket
import tempfile
import threading
import unittest
from unittest.mock import patch

from warden.core import dumps
from warden.sbx import Registry, NetworkProof, binding_digest, handler, Server, rpc


def run_context(**changes):
    return {'projectID': 'p1', 'sandboxID': 's1', 'runtimeName': 'sbx-one',
            'generation': '1', 'chatID': 'c1', 'runID': 'r1', 'principalID': 'owner', **changes}


class FixtureVerifier:
    """Synthetic proof exists in tests only; production has no enable API."""
    def __init__(self):
        self.registry = None
        self.enabled = True
        self.overrides = {}

    def verify(self, binding, phase):
        if not self.enabled:
            return None
        actual = self.registry.bindings[binding['sandboxID']]
        return NetworkProof(**{'binding': binding_digest(binding), 'phase': phase,
            'policy_digest': hashlib.sha256(dumps(actual['engine'].policy).encode()).hexdigest(),
            'expires_at': self.registry.clock() + 15, 'evidence_id': 'synthetic-fixture',
            'gateway_port': actual['gateway_port'], **self.overrides})


class SbxTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.now = 1000
        self.verifier = FixtureVerifier()
        self.registry = Registry(self.tmp.name, verifier=self.verifier, clock=lambda: self.now)
        self.verifier.registry = self.registry
        self.value = run_context()
        self.registry.register(self.value)
        self.registry.bind_gateway(self.value, 19443)
        self.registry.configure_provider(self.value, 'synthetic-secret-for-tests-only')
        self.gateway = self.registry.gateway(self.value)

    def tearDown(self):
        self.registry.close()
        self.tmp.cleanup()

    def proxy(self, message, gateway=None):
        gateway = gateway or self.gateway
        return self.registry.proxy(gateway['bindingID'], gateway['capability'], message)

    def egress(self, **changes):
        request = {'host': 'api.openai.com', 'method': 'POST', 'scheme': 'https',
                   'path': '/v1/responses', **changes}
        return request, self.proxy({'action': 'egress', 'request': request})

    def test_default_verifier_never_accepts_registration_or_static_claim(self):
        from warden.sbx import UnsupportedVerifier
        self.registry.verifier = UnsupportedVerifier()
        self.assertFalse(self.registry.check(self.value, 'create')['ready'])
        self.assertFalse(self.registry.check(self.value, 'runtime')['ready'])
        self.assertFalse(self.registry.begin(self.value)['ready'])
        with self.assertRaises(ValueError):
            self.registry.dispatch({'version': 1, 'operation': 'check', 'phase': 'runtime',
                                    'context': self.value, 'ready': True})

    def test_bindings_immutable_across_project_runtime_generation_principal(self):
        for field in ('projectID', 'runtimeName', 'principalID'):
            with self.assertRaises(ValueError):
                self.registry.register({**self.value, field: 'changed'})
        with self.assertRaises(ValueError):
            self.registry.register(run_context(sandboxID='another'))
        self.assertTrue(self.registry.register(self.value)['ok'])

    def test_generation_replacement_requires_ended_lease_and_rejects_replay(self):
        updated = run_context(generation='2', runID='r2')
        self.registry.begin(self.value)
        with self.assertRaises(ValueError): self.registry.register(updated)
        self.registry.end(self.value)
        self.assertTrue(self.registry.register(updated)['ok'])
        self.assertNotEqual(self.registry.gateway(updated)['capability'], self.gateway['capability'])
        self.verifier.overrides = {'binding': binding_digest(self.value)}
        self.assertFalse(self.registry.check(updated, 'runtime')['ready'])
        with self.assertRaises(ValueError): self.registry.register(self.value)
        self.registry.close()
        self.registry = Registry(self.tmp.name)
        with self.assertRaises(ValueError): self.registry.register(self.value)

    def test_identity_survives_restart_but_credentials_and_capabilities_do_not(self):
        self.registry.begin(self.value)
        self.registry.close()
        self.registry = Registry(self.tmp.name)
        self.assertFalse(self.registry.check(self.value, 'runtime')['ready'])
        self.assertIsNone(self.registry.bindings['s1']['provider_secret'])
        self.assertIsNone(self.registry.bindings['s1']['lease'])
        self.assertEqual(self.registry.bindings['s1']['gateway_port'], 19443)
        self.assertNotEqual(self.registry.gateway(self.value)['capability'], self.gateway['capability'])
        with self.assertRaises(ValueError):
            self.proxy({'action': 'egress', 'request': {}})

    def test_wrong_or_stale_proof_cannot_start(self):
        for override in ({'binding': 'wrong'}, {'phase': 'create'}, {'policy_digest': 'wrong'},
                         {'expires_at': self.now}, {'expires_at': self.now + 60},
                         {'gateway_port': 19444}, {'evidence_id': ''}):
            self.verifier.overrides = override
            self.assertFalse(self.registry.begin(self.value)['ready'], override)

    def test_context_and_protocol_reject_unknown_fields(self):
        for message in ({'version': True, 'operation': 'end', 'context': self.value},
                        {'version': 1, 'operation': 'end', 'context': {**self.value, 'authority': 'admin'}},
                        {'version': 1, 'operation': 'end', 'context': {**self.value, 'sandboxID': '../s1'}}):
            with self.assertRaises(ValueError): self.registry.dispatch(message)

    def test_provider_requires_verified_lease_and_returns_only_placeholder_to_worker(self):
        self.assertFalse(self.egress()[1]['allow'])
        result = self.registry.begin(self.value)
        self.assertTrue(result['ready'])
        self.assertEqual(result['apiKeyPlaceholder'], 'warden-proxy-managed')
        self.assertEqual(result['providerBaseURL'], 'http://host.docker.internal:19443/openai/v1')
        self.assertNotIn('synthetic-secret', dumps(result))
        request, decision = self.egress()
        result = self.proxy({'action': 'provider', 'decision_id': decision['decision_id'], 'request': request})
        self.assertEqual(result['authorization'], 'Bearer synthetic-secret-for-tests-only')

    def test_provider_exact_route_and_record_binding(self):
        self.registry.begin(self.value)
        request, decision = self.egress()
        for changes in ({'host': 'example.com'}, {'path': '/v1/files'}, {'path': '/v1/responses?redirect=evil'},
                        {'scheme': 'http'}, {'method': 'GET'}):
            self.assertFalse(self.proxy({'action': 'provider', 'decision_id': decision['decision_id'],
                'request': {**request, **changes}})['allow'])
        for path in ('/v1/files', '/v1/responses?x=y'):
            req, result = self.egress(path=path)
            self.assertFalse(self.proxy({'action': 'provider', 'decision_id': result['decision_id'], 'request': req})['allow'])

    def test_gateway_capability_cannot_cross_sandbox(self):
        other = run_context(sandboxID='s2', runtimeName='sbx-two')
        self.registry.register(other)
        with self.assertRaises(ValueError):
            self.registry.proxy('s2', self.gateway['capability'], {'action': 'egress', 'request': {}})
        with self.assertRaises(ValueError):
            self.proxy({'action': 'ready', 'firewall': 'enforced'})
        with self.assertRaises(ValueError):
            self.proxy({'action': 'approve', 'request_id': 'forged'})

    def test_end_and_expiry_revoke_decisions_and_inflight_active(self):
        self.registry.begin(self.value)
        request, decision = self.egress()
        self.registry.end(self.value)
        self.assertFalse(self.proxy({'action': 'active', 'decision_id': decision['decision_id']})['active'])
        self.assertFalse(self.proxy({'action': 'provider', 'decision_id': decision['decision_id'], 'request': request})['allow'])
        self.assertFalse(self.registry.begin(self.value)['ready'])
        next_run = run_context(runID='r2')
        self.assertTrue(self.registry.begin(next_run)['ready'])
        self.now += 121
        self.assertFalse(self.egress()[1]['allow'])
        self.assertFalse(self.registry.begin(next_run, renew=True)['ready'])

    def test_other_chat_cannot_end_active_run_and_renewal_is_bounded(self):
        self.registry.begin(self.value)
        other = run_context(chatID='c2', runID='r2')
        self.assertFalse(self.registry.begin(other)['ready'])
        self.assertFalse(self.registry.end(other)['ok'])
        self.now += 30
        self.assertTrue(self.registry.begin(self.value, renew=True)['ready'])
        self.assertEqual(self.registry.bindings['s1']['lease']['expires_at'], self.now + 120)

    def test_loss_of_readiness_revokes_lease_and_old_decisions(self):
        self.registry.begin(self.value)
        request, decision = self.egress()
        self.verifier.enabled = False
        self.assertFalse(self.registry.check(self.value, 'runtime')['ready'])
        self.verifier.enabled = True
        self.assertFalse(self.proxy({'action': 'provider', 'decision_id': decision['decision_id'], 'request': request})['allow'])

    def test_audit_failure_never_activates_or_releases_provider_credential(self):
        engine = self.registry.bindings['s1']['engine']
        with patch.object(engine.audit, 'emit', side_effect=OSError('ENOSPC')):
            with self.assertRaises(OSError): self.registry.begin(self.value)
        self.assertIsNone(self.registry.bindings['s1']['lease'])
        self.registry.begin(self.value)
        request, decision = self.egress()
        with patch.object(engine.audit, 'emit', side_effect=OSError('ENOSPC')):
            with self.assertRaises(OSError):
                self.proxy({'action': 'provider', 'decision_id': decision['decision_id'], 'request': request})

    def test_manifest_failure_poisoning_prevents_in_memory_retry_readiness(self):
        with patch.object(self.registry, '_save_manifest', side_effect=OSError('ENOSPC')):
            with self.assertRaises(OSError):
                self.registry.register(run_context(sandboxID='s2', runtimeName='sbx-two'))
        self.assertFalse(self.registry.check(self.value, 'runtime')['ready'])
        with self.assertRaises(ValueError):
            self.registry.dispatch({'version': 1, 'operation': 'check', 'context': self.value, 'phase': 'runtime'})

    def test_credentials_absent_from_durable_state_and_opaque_tls_denied(self):
        self.registry.begin(self.value)
        self.assertFalse(self.proxy({'action': 'egress', 'request': {
            'host': 'swcdn.apple.com', 'method': 'GET', 'scheme': 'https', 'tls': True}})['allow'])
        for path in Path(self.tmp.name).rglob('*'):
            if path.is_file(): self.assertNotIn(b'synthetic-secret-for-tests-only', path.read_bytes())

    def test_private_protocol_roundtrip_duplicate_json_and_generic_errors(self):
        path = Path(self.tmp.name) / 'control.sock'
        with Server(str(path), handler(self.registry)) as server:
            path.chmod(0o600)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                result = rpc(path, {'version': 1, 'operation': 'check', 'context': self.value, 'phase': 'runtime'})
                self.assertTrue(result['ready'])
                with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
                    conn.connect(str(path))
                    conn.sendall(b'{"version":1,"version":1,"secret":"never-echo"}\n')
                    result = conn.recv(4096)
                    self.assertNotIn(b'never-echo', result)
                    self.assertFalse(json.loads(result)['ready'])
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            finally:
                server.shutdown()


if __name__ == '__main__':
    unittest.main()
