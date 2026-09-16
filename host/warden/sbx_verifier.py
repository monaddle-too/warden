"""Narrow SBX 0.42.1 host verifier and owned-gateway lifecycle.

Trusts the app worker's fixed mountless shell creation contract. It independently
checks actual daemon policy, identity and gateway liveness; no guest report or
worker-provided ready flag contributes evidence. Hypervisor/security bugs and
configuration races caused by other trusted host processes remain outside this
bounded verification, as with the existing Warden host trust model.
"""
from __future__ import annotations

import hashlib
import hmac
import http.client
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import threading
import time

from .core import dumps, strict_json
from .sbx import binding_digest
from .sbx_types import NetworkProof
from .sbx_gateway import command

VERSION = 'v0.42.1'
SHELL_DIGEST = 'sha256:5fc81bc7a127e59d81b244a06831ae3212a0310b2e5a0349c54e29249e45e919'


class GatewayPool:
    def __init__(self, registry, executable, socket_path):
        self.registry, self.executable, self.socket_path = registry, executable, socket_path
        self.processes = {}

    def ensure(self, binding):
        sandbox = binding['identity']['sandboxID']
        current = self.processes.get(sandbox)
        signature = binding_digest(binding['identity'])
        if current and current[0] == signature and current[1].poll() is None:
            return
        if current:
            self._stop(current)
            del self.processes[sandbox]
        if binding['gateway_port'] is None:
            with socket.socket() as listener:
                listener.bind(('127.0.0.1', 0))
                port = listener.getsockname()[1]
            self.registry._set_gateway_port(binding, port)
        root = binding['engine'].state / 'gateway'
        root.mkdir(mode=0o700, exist_ok=True)
        config = root / 'config.json'
        config.write_text(dumps({'socketPath': str(self.socket_path), 'bindingID': sandbox,
                                'capability': binding['capability'], 'gatewayPort': binding['gateway_port']}))
        config.chmod(0o600)
        log = (root / 'process.log').open('w')
        env = {**os.environ, 'WARDEN_SBX_GATEWAY_CONFIG': str(config),
               'WARDEN_SBX_PARENT_PID': str(os.getpid())}
        process = subprocess.Popen(command(self.executable, root, binding['gateway_port']),
                                   env=env, stdout=log, stderr=log)
        self.processes[sandbox] = (signature, process, log)
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and process.poll() is None:
            if gateway_healthy(binding): return
            time.sleep(0.05)
        raise ValueError('managed gateway failed to become ready')

    def _stop(self, current):
        process = current[1]
        if process.poll() is None:
            process.terminate()
            try: process.wait(timeout=3)
            except subprocess.TimeoutExpired: process.kill();process.wait(timeout=3)
        current[2].close()

    def close(self):
        for current in self.processes.values(): self._stop(current)
        self.processes.clear()


def gateway_healthy(binding):
    nonce = secrets.token_hex(32)
    conn = http.client.HTTPConnection('127.0.0.1', binding['gateway_port'], timeout=1)
    try:
        conn.request('GET', '/__warden_sbx_health/' + nonce)
        response = conn.getresponse()
        value = strict_json(response.read(4097))
        expected = hmac.new(binding['capability'].encode(), nonce.encode(), hashlib.sha256).hexdigest()
        return (response.status == 200 and value.get('bindingID') == binding['identity']['sandboxID']
                and isinstance(value.get('mac'), str) and hmac.compare_digest(value['mac'], expected))
    except Exception:
        return False
    finally:
        conn.close()


PROOF_SECONDS = 20      # host proof lifetime; unchanged policy
REFRESH_SECONDS = 12    # background re-verification cadence for warm bindings (inspection takes ~5 s)
WARM_SECONDS = 600      # a binding stays warm this long after its last begin


class SbxCliVerifier:
    def __init__(self, registry, executable, manage_network=False, runner=None):
        self.registry, self.executable = registry, executable
        self.manage_network = manage_network
        self.runner = runner or self._run
        self.pins_path = registry.state / 'runtime-identities.json'
        self.pins = strict_json(self.pins_path.read_text()) if self.pins_path.exists() else {}
        self.cache = {}
        self.last_failure = None
        # CLI inspection never runs concurrently; the registry lock is not held
        # while it runs in the background, so chats are not blocked by it.
        self.verify_lock = threading.Lock()
        self._stop = threading.Event()
        self._thread = None

    # Background refresh: keeps proofs for warm bindings fresh off the chat path.
    def start_refresher(self):
        if self._thread is None:
            self._thread = threading.Thread(target=self._refresh_loop, name='warden-proof-refresher', daemon=True)
            self._thread.start()

    def stop_refresher(self):
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=10)
            self._thread = None

    def _refresh_loop(self):
        while not self._stop.wait(REFRESH_SECONDS):
            try:
                self.refresh_once()
            except Exception:
                pass

    def _warm(self, binding):
        now = self.registry.clock()
        lease = binding.get('lease')
        if lease and now < lease['expires_at']:
            return True
        last = binding.get('last_begin')
        return last is not None and now < last + WARM_SECONDS

    def refresh_once(self):
        with self.registry.lock:
            targets = [dict(b['identity']) for b in self.registry.bindings.values() if self._warm(b)]
        refreshed = 0
        for identity in targets:
            with self.verify_lock:
                try:
                    self._verify(identity, 'runtime', force=True)
                    self.last_failure = None
                    refreshed += 1
                except Exception as error:
                    self._failed(identity, error)
        return refreshed

    def _fresh(self, identity, phase):
        binding = self.registry.bindings.get(identity['sandboxID'])
        if binding is None or binding['identity'] != identity:
            return None
        policy_digest = hashlib.sha256(dumps(binding['engine'].policy).encode()).hexdigest()
        cached = self.cache.get((binding_digest(identity), phase, policy_digest))
        if cached and self.registry.clock() < cached.expires_at and gateway_healthy(binding):
            return cached
        return None

    def _run(self, args, denied=False):
        result = subprocess.run([self.executable, *args], capture_output=True, timeout=4)
        if (result.returncode != 0 and not (denied and result.returncode == 1)) or len(result.stdout) > 1024*1024:
            raise ValueError('SBX inspection failed')
        return result.stdout.decode()

    def _json(self, args, denied=False):
        return strict_json(self.runner(args, denied))

    def _rules(self, name=None):
        args = ['policy', 'ls'] + ([name] if name else []) + ['--json', '--include-inactive']
        result = self._json(args)
        if set(result) != {'rules'} or not isinstance(result['rules'], list):
            raise ValueError('unrecognized policy schema')
        relevant = []
        for rule in result['rules']:
            kind = rule.get('resource_type')
            if kind in ('filesystem:read', 'filesystem:write'): continue
            if kind != 'network' or rule.get('status') != 'active' or rule.get('layer') != 'local':
                raise ValueError('unknown or governed network policy')
            # Linux SBX 0.42.1 materializes its immutable implicit-deny
            # sentinel in policy ls. It grants nothing; _check still proves
            # implicit denial and absence of governance independently.
            if rule == {
                'id': 'default-deny-all', 'name': 'default-deny-all',
                'policy_name': 'default-deny-all', 'scope': 'global',
                'applies_to': 'all', 'resource_type': 'network',
                'decision': 'deny', 'resources': ['**'], 'origin': 'local',
                'layer': 'local', 'status': 'active', 'editable': False,
            }: continue
            if name is None and rule.get('scope') != 'global': continue
            relevant.append(rule)
        return relevant

    def _check(self, name, target, allowed):
        args = ['policy', 'check', 'network', '--json'] + (['--sandbox', name] if name else []) + [target]
        result = self._json(args, denied=not allowed)
        if result.get('allowed') is not allowed or result.get('governance') != {'active': False}:
            raise ValueError('policy result or governance mismatch')
        if not allowed and result.get('deny_kind') != 'implicit':
            raise ValueError('default-deny policy required')

    def _pin(self, name, uuid):
        if not isinstance(uuid, str) or not uuid:
            raise ValueError('missing runtime UUID')
        if name in self.pins:
            if self.pins[name] != uuid: raise ValueError('runtime UUID changed')
            return
        updated = {**self.pins, name: uuid}
        path = self.pins_path.with_suffix('.tmp')
        try:
            with path.open('w') as stream:
                path.chmod(0o600);stream.write(dumps(updated));stream.flush();os.fsync(stream.fileno())
            os.replace(path, self.pins_path)
            directory = os.open(self.registry.state, os.O_RDONLY)
            try: os.fsync(directory)
            finally: os.close(directory)
        except Exception:
            self.registry.storage_failed = True
            raise
        self.pins = updated

    def verify(self, identity, phase):
        # A fresh proof with a healthy gateway needs no CLI inspection now.
        cached = self._fresh(identity, phase)
        if cached: return cached
        with self.verify_lock:
            cached = self._fresh(identity, phase)
            if cached: return cached
            try:
                proof = self._verify(identity, phase)
                self.last_failure = None
                return proof
            except Exception as error:
                self._failed(identity, error)
                raise

    def _failed(self, identity, error):
        self.last_failure = str(error)[:160] if isinstance(error, ValueError) else type(error).__name__
        self.cache.clear()
        if self.manage_network and identity['runtimeName'] in self.pins:
            try:
                self.runner(['policy', 'deny', 'network', '--sandbox', identity['runtimeName'], '**'], False)
            except Exception:
                pass  # Worker must also stop on failed readiness; no success is reported.

    def _verify(self, identity, phase, force=False):
        binding = self.registry.bindings[identity['sandboxID']]
        self.registry.gateway_pool.ensure(binding)
        if not gateway_healthy(binding): raise ValueError('gateway unavailable')
        policy_digest = hashlib.sha256(dumps(binding['engine'].policy).encode()).hexdigest()
        cache_key = (binding_digest(identity), phase, policy_digest)
        cached = self.cache.get(cache_key)
        # Recheck host policy frequently without spawning a CLI per audit event.
        if cached and not force and self.registry.clock() < cached.expires_at: return cached
        version = self.runner(['version'], False).strip()
        if not version.startswith('sbx version: ' + VERSION + ' '): raise ValueError('unsupported SBX version')
        for key, required in (('ssh.agentForwardingEnabled', False), ('proxy.sandbox', 'direct')):
            setting = self._json(['settings', 'get', '--json', key])
            if setting.get('key') != key or type(setting.get('value')) is not type(required) or setting.get('value') != required:
                raise ValueError('unsafe SBX setting')
        mcp = self._json(['mcp', 'ls', '--json'])
        if mcp.get('servers') != [] or mcp.get('gateway', {}).get('local') is not True:
            raise ValueError('host MCP gateway must have no servers')
        if self._rules(): raise ValueError('global network permissions unsupported')
        self._check(None, 'example.com:443', False)
        rows = self._json(['ls', '--json']).get('sandboxes')
        if not isinstance(rows, list): raise ValueError('invalid runtime inventory')
        matching = [row for row in rows if row.get('name') == identity['runtimeName']]
        if len(matching) > 1: raise ValueError('ambiguous runtime identity')
        if phase == 'runtime' and not matching: raise ValueError('runtime absent')
        if matching:
            row = matching[0]
            if row.get('agent') != 'shell' or row.get('status') not in ('running', 'stopped'):
                raise ValueError('unsupported runtime')
            details = self._json(['inspect', identity['runtimeName'], '--json'])
            if (details.get('daemon_version') != VERSION or details.get('agent') != 'shell'
                    or details.get('image_digest') != SHELL_DIGEST or details.get('kits') != []
                    or any(details.get(k) for k in ('workspace', 'workspaces', 'mounts', 'static_mcp'))):
                raise ValueError('unsupported runtime profile')
            self._pin(identity['runtimeName'], row.get('id'))
        if phase == 'runtime':
            name, resource = identity['runtimeName'], 'localhost:' + str(binding['gateway_port'])
            rules = self._rules(name)
            bootstrap = []
            allowed = []
            for rule in rules:
                if rule.get('scope') != 'sandbox:' + name or rule.get('sandbox_id') != name or rule.get('origin') != 'scoped':
                    raise ValueError('policy outside owned sandbox')
                if rule.get('decision') == 'deny' and rule.get('resources') == ['**']:
                    bootstrap.append(rule)
                elif rule.get('decision') == 'allow' and rule.get('resources') == [resource]:
                    allowed.append(rule)
                else: raise ValueError('unexpected sandbox permission')
            if self.manage_network and bootstrap:
                if not allowed:
                    self.runner(['policy', 'allow', 'network', '--sandbox', name, resource], False)
                # Deny remains active while the sole gateway exception is added.
                self.runner(['policy', 'rm', 'network', '--sandbox', name, '--resource', '**'], False)
                rules = self._rules(name)
                if len(rules) != 1 or rules[0].get('decision') != 'allow' or rules[0].get('resources') != [resource]:
                    raise ValueError('gateway policy transition failed')
            elif bootstrap or len(allowed) != 1:
                raise ValueError('gateway-only policy not established')
            self._check(name, resource, True)
            for target in ('example.com:443', 'api.openai.com:443', '1.1.1.1:443', 'localhost:18765'):
                self._check(name, target, False)
        proof = NetworkProof(binding_digest(identity), phase, policy_digest,
                             self.registry.clock() + PROOF_SECONDS, 'sbx-0.42.1-cli-and-gateway', binding['gateway_port'])
        self.cache[cache_key] = proof
        return proof
