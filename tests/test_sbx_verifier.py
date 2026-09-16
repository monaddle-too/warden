import json
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from warden.sbx import Registry
from warden.sbx_verifier import SbxCliVerifier, SHELL_DIGEST
from test_sbx import run_context


class CliFixture:
    def __init__(self):
        self.commands = []
        self.runtime = True
        self.uuid = 'fixture-runtime-uuid'
        self.mcp = []
        self.ssh = False
        self.image = SHELL_DIGEST
        self.network = [self.rule('deny', '**')]
        self.global_network = []

    def rule(self, decision, resource):
        return {'resource_type': 'network', 'status': 'active', 'layer': 'local',
                'scope': 'sandbox:sbx-one', 'sandbox_id': 'sbx-one', 'origin': 'scoped',
                'decision': decision, 'resources': [resource]}

    def __call__(self, args, denied=False):
        self.commands.append(args)
        if args == ['version']: return 'sbx version: v0.42.1 fixture-build'
        if args[:3] == ['settings', 'get', '--json']:
            return json.dumps({'key': args[3], 'value': self.ssh if args[3].startswith('ssh.') else 'direct'})
        if args[:2] == ['mcp', 'ls']: return json.dumps({'gateway': {'local': True}, 'servers': self.mcp})
        if args[:2] == ['policy', 'ls']:
            return json.dumps({'rules': self.global_network + (self.network if 'sbx-one' in args else [])})
        if args[:3] == ['policy', 'check', 'network']:
            allow = '--sandbox' in args and args[-1] == 'localhost:19443'
            return json.dumps({'allowed': allow, 'governance': {'active': False}, **({} if allow else {'deny_kind': 'implicit'})})
        if args == ['ls', '--json']:
            return json.dumps({'sandboxes': [{'name': 'sbx-one', 'id': self.uuid, 'agent': 'shell', 'status': 'stopped'}] if self.runtime else []})
        if args[0] == 'inspect':
            return json.dumps({'daemon_version': 'v0.42.1', 'agent': 'shell', 'image_digest': self.image, 'kits': []})
        if args[:3] == ['policy', 'allow', 'network']:
            self.network.append(self.rule('allow', args[-1]));return '{}'
        if args[:3] == ['policy', 'rm', 'network']:
            self.network = [r for r in self.network if r['resources'] != [args[-1]]];return '{}'
        if args[:3] == ['policy', 'deny', 'network']:
            if self.rule('deny', args[-1]) not in self.network: self.network.append(self.rule('deny', args[-1]))
            return '{}'
        raise AssertionError(args)


class VerifierTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.now = 1000
        self.registry = Registry(self.tmp.name, clock=lambda: self.now)
        self.registry.gateway_pool = SimpleNamespace(ensure=Mock(), close=Mock())
        self.value = run_context()
        self.registry.register(self.value)
        self.registry.bind_gateway(self.value, 19443)
        self.cli = CliFixture()
        self.verifier = SbxCliVerifier(self.registry, '/trusted/sbx', True, runner=self.cli)
        self.registry.verifier = self.verifier
        self.health = patch('warden.sbx_verifier.gateway_healthy', return_value=True)
        self.health.start()

    def tearDown(self):
        self.health.stop()
        self.registry.close()
        self.tmp.cleanup()

    def test_create_proves_global_deny_without_executing_guest_or_changing_policy(self):
        self.cli.runtime = False
        self.assertTrue(self.registry.check(self.value, 'create')['ready'])
        self.assertFalse(any(args[0] in ('create', 'exec', 'run') for args in self.cli.commands))
        self.assertFalse(any(args[:2] in (['policy', 'allow'], ['policy', 'rm']) for args in self.cli.commands))

    def test_scoped_transition_adds_allow_before_removing_bootstrap_deny(self):
        self.assertTrue(self.registry.check(self.value, 'runtime')['ready'])
        mutations = [args for args in self.cli.commands if args[:2] in (['policy', 'allow'], ['policy', 'rm'])]
        self.assertEqual(mutations, [
            ['policy', 'allow', 'network', '--sandbox', 'sbx-one', 'localhost:19443'],
            ['policy', 'rm', 'network', '--sandbox', 'sbx-one', '--resource', '**']])

    def test_linux_immutable_default_deny_sentinel(self):
        sentinel = {'id': 'default-deny-all', 'name': 'default-deny-all',
                    'policy_name': 'default-deny-all', 'scope': 'global',
                    'applies_to': 'all', 'resource_type': 'network',
                    'decision': 'deny', 'resources': ['**'], 'origin': 'local',
                    'layer': 'local', 'status': 'active', 'editable': False}
        self.cli.global_network = [sentinel]
        self.assertTrue(self.registry.check(self.value, 'runtime')['ready'])
        self.now += 21
        self.cli.global_network = [{**sentinel, 'decision': 'allow'}]
        self.assertFalse(self.registry.check(self.value, 'runtime')['ready'])

    def test_unmanaged_bootstrap_stays_closed(self):
        self.verifier.manage_network = False
        self.assertFalse(self.registry.check(self.value, 'runtime')['ready'])

    def test_runtime_uuid_replacement_rejected_after_cache_expires(self):
        self.assertTrue(self.registry.check(self.value, 'runtime')['ready'])
        self.now += 21
        self.cli.uuid = 'different-runtime'
        self.assertFalse(self.registry.check(self.value, 'runtime')['ready'])
        self.assertEqual(self.cli.commands[-1], ['policy', 'deny', 'network', '--sandbox', 'sbx-one', '**'])

    def test_unexpected_permissions_mcp_or_forwarding_rejected(self):
        for attribute, value in (('mcp', [{'name': 'host-shell'}]), ('ssh', True), ('image', 'sha256:wrong'),
                                 ('network', [self.cli.rule('allow', '**')]),
                                 ('global_network', [{**self.cli.rule('allow', '**'), 'scope': 'global'}])):
            original = getattr(self.cli, attribute)
            setattr(self.cli, attribute, value)
            self.assertFalse(self.registry.check(self.value, 'runtime')['ready'], attribute)
            setattr(self.cli, attribute, original)

    def test_reuses_attestation_until_twenty_second_expiry(self):
        self.assertTrue(self.registry.check(self.value, "runtime")["ready"])
        count = len(self.cli.commands)
        self.now += 19
        self.assertTrue(self.registry.check(self.value, "runtime")["ready"])
        self.assertEqual(len(self.cli.commands), count)
        self.now += 2
        self.assertTrue(self.registry.check(self.value, "runtime")["ready"])
        self.assertGreater(len(self.cli.commands), count)

    def test_live_gateway_health_is_required_even_with_cached_host_proof(self):
        self.assertTrue(self.registry.check(self.value, 'runtime')['ready'])
        with patch('warden.sbx_verifier.gateway_healthy', return_value=False):
            self.assertFalse(self.registry.check(self.value, 'runtime')['ready'])


if __name__ == '__main__':
    unittest.main()


class RefresherTests(unittest.TestCase):
    tearDown = VerifierTests.tearDown

    def setUp(self):
        VerifierTests.setUp(self)
        self.registry.configure_provider(self.value, 'synthetic-secret-for-tests-only')

    def cli_calls(self):
        return [args for args in self.cli.commands if args[:2] not in (['policy', 'deny'],)]

    def test_warm_binding_is_reverified_in_background_and_chat_path_uses_cache(self):
        self.assertTrue(self.registry.check(self.value, 'runtime')['ready'])
        self.assertTrue(self.registry.begin(self.value)['ready'])
        self.now += 12  # refresher cadence; the first proof would expire at +20
        self.cli.commands.clear()
        self.assertEqual(self.verifier.refresh_once(), 1)
        self.assertTrue(self.cli_calls(), 'background refresh must inspect the host')
        self.cli.commands.clear()
        self.now += 9  # +21: only the refreshed proof is still valid
        self.assertTrue(self.registry.begin(self.value, renew=True)['ready'])
        self.assertTrue(self.registry.check(self.value, 'runtime')['ready'])
        self.assertEqual(self.cli_calls(), [], 'chat path must not run the CLI while the proof is fresh')

    def test_cold_binding_is_not_refreshed(self):
        self.assertEqual(self.verifier.refresh_once(), 0)
        self.assertEqual(self.cli.commands, [])
        self.assertTrue(self.registry.check(self.value, 'runtime')['ready'])
        self.now += 700  # warm window elapsed without a begin
        self.cli.commands.clear()
        self.assertEqual(self.verifier.refresh_once(), 0)
        self.assertEqual(self.cli.commands, [])

    def test_stale_proof_still_verifies_synchronously(self):
        self.assertTrue(self.registry.begin(self.value)['ready'])
        self.now += 21
        self.cli.commands.clear()
        self.assertTrue(self.registry.begin(self.value, renew=True)['ready'])
        self.assertTrue(self.cli_calls())

    def test_failed_background_verification_revokes_like_a_synchronous_one(self):
        self.assertTrue(self.registry.begin(self.value)['ready'])
        self.cli.mcp = [{'name': 'rogue'}]
        self.assertEqual(self.verifier.refresh_once(), 0)
        self.assertIn('MCP', self.verifier.last_failure)
        self.assertFalse(self.registry.check(self.value, 'runtime')['ready'])
        self.assertIn(['policy', 'deny', 'network', '--sandbox', 'sbx-one', '**'], self.cli.commands)

    def test_unhealthy_gateway_bypasses_fresh_cache(self):
        self.assertTrue(self.registry.begin(self.value)['ready'])
        self.cli.commands.clear()
        self.health.stop()
        with patch('warden.sbx_verifier.gateway_healthy', return_value=False):
            self.assertFalse(self.registry.check(self.value, 'runtime')['ready'])
        self.health = patch('warden.sbx_verifier.gateway_healthy', return_value=True)
        self.health.start()
