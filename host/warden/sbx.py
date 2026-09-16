"""Private SBX control and credential broker; no production network verifier yet.

The socket is host-only. Gateway RPC is additionally bound to a host-held random
capability. A guest never supplies the context used for authorization. Installing
this service alone deliberately cannot enable sandbox execution.
"""
from __future__ import annotations

import argparse
import base64
import fcntl
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import signal
import socket
import socketserver
import stat
import threading
import time

from .core import Engine, dumps, strict_json
from .server import LimitedThreads, internal
from .sbx_types import NetworkProof

CONTEXT = frozenset(('projectID', 'sandboxID', 'runtimeName', 'generation',
                     'chatID', 'runID', 'principalID'))
IDENTITY = ('projectID', 'sandboxID', 'runtimeName', 'generation', 'principalID')
ID = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}')
MAX_MESSAGE = 12 * 1024 * 1024
LEASE_SECONDS = 120
PROVIDER_ROUTES = frozenset(('/v1/responses', '/v1/chat/completions', '/v1/messages', '/v1/messages/count_tokens'))


def context(value):
    if not isinstance(value, dict) or set(value) not in (CONTEXT, CONTEXT | {'provider'}):
        raise ValueError('invalid context')
    if value.get('provider', 'codex') not in ('codex', 'claude'): raise ValueError('invalid provider')
    if any(not isinstance(v, str) or not ID.fullmatch(v) for v in value.values()):
        raise ValueError('invalid context identifier')
    return dict(value)


def identity(value):
    return {key: value[key] for key in IDENTITY}


def binding_digest(value):
    return hashlib.sha256(dumps(identity(value)).encode()).hexdigest()


class UnsupportedVerifier:
    def verify(self, binding, phase):
        return None


class Registry:
    def __init__(self, state, verifier=None, clock=time.monotonic, provider_source=None, document_api=None):
        self.state = Path(state)
        self.state.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.state.chmod(0o700)
        self.verifier = verifier or UnsupportedVerifier()
        self.provider_source = provider_source
        self.claude_source = None
        self.document_api = document_api
        self.clock = clock
        self.lock = threading.RLock()
        self.bindings = {}
        self.storage_failed = False
        self.retired = {}
        manifest = self.state / 'bindings.json'
        saved = strict_json(manifest.read_text()) if manifest.exists() else {'active': [], 'retired': {}}
        if set(saved) != {'active', 'retired'}:
            raise ValueError('invalid binding manifest')
        self.retired = saved['retired']
        for value in saved['active']:
            if set(value) != set(IDENTITY):
                raise ValueError('invalid durable binding')
            if value['generation'] in self.retired.get(value['sandboxID'], []):
                raise ValueError('retired durable binding')
            self._load(value)

    def _save(self):
        try:
            self._save_manifest()
        except Exception:
            # A failed durable transition cannot be repaired by a successful
            # in-memory idempotent retry. Require restart/reconciliation.
            self.storage_failed = True
            raise

    def _save_manifest(self):
        path = self.state / 'bindings.json'
        temporary = self.state / 'bindings.tmp'
        with temporary.open('w') as stream:
            temporary.chmod(0o600)
            stream.write(dumps({'active': [b['identity'] for b in self.bindings.values()],
                                'retired': self.retired}) + '\n')
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        descriptor = os.open(self.state, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)

    def _load(self, value):
        sandbox = value['sandboxID']
        if sandbox in self.bindings:
            raise ValueError('duplicate sandbox identity')
        for other in self.bindings.values():
            if other['identity']['runtimeName'] == value['runtimeName']:
                raise ValueError('runtime already registered')
        directory = self.state / 'sandboxes' / binding_digest(value)
        directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        engine = Engine(directory)
        # Repository-less chats must not inherit account-wide GitHub authority.
        try:
            if 'allowed_repositories' not in engine.policy:
                engine.save_policy({**engine.policy, 'allowed_repositories': []})
        except Exception:
            engine.db.close()
            os.close(engine.audit.fd)
            raise
        port_path = directory / 'gateway-port.json'
        port = strict_json(port_path.read_text()) if port_path.exists() else None
        if port is not None and (type(port) is not int or not 1024 <= port <= 65535):
            engine.db.close();os.close(engine.audit.fd)
            raise ValueError('invalid durable gateway port')
        self.bindings[sandbox] = {
            'identity': dict(value), 'engine': engine,
            'capability': secrets.token_urlsafe(32), 'lease': None,
            'ended': set(), 'decisions': {}, 'provider_secret': None,
            'gateway_port': port,
        }
        return self.bindings[sandbox]

    def register(self, value):
        value = context(value)
        with self.lock:
            if value['sandboxID'] in self.bindings:
                previous = self.bindings[value['sandboxID']]
                if identity(value) == previous['identity']:
                    return {'ok': True, 'ready': False}
                if any(value[k] != previous['identity'][k] for k in IDENTITY if k != 'generation'):
                    raise ValueError('immutable sandbox identity changed')
                if self._lease(previous):
                    raise ValueError('cannot replace an active generation')
                retired = self.retired.setdefault(value['sandboxID'], [])
                if value['generation'] in retired or len(retired) >= 10000:
                    raise ValueError('generation cannot be reused')
                self._revoke(previous)
                retired.append(previous['identity']['generation'])
                del self.bindings[value['sandboxID']]
                replacement = self._load(identity(value))
                replacement['engine'].save_policy(previous['engine'].policy)
                replacement['provider_secret'] = previous['provider_secret']
                if previous['gateway_port'] is not None:
                    self._set_gateway_port(replacement, previous['gateway_port'])
                replacement['engine'].redactor.register(previous['provider_secret'])
                self._save()
                previous['engine'].db.close()
                os.close(previous['engine'].audit.fd)
                return {'ok': True, 'ready': False}
            if len(self.bindings) >= 64:
                raise ValueError('sandbox registry full')
            for other in self.bindings.values():
                if other['identity']['runtimeName'] == value['runtimeName']:
                    raise ValueError('runtime already registered')
            self._load(identity(value))
            self._save()
            return {'ok': True, 'ready': False}

    def _binding(self, value):
        binding = self.bindings.get(value['sandboxID'])
        if not binding or binding['identity'] != identity(value):
            raise ValueError('binding mismatch')
        return binding

    def _proof(self, binding, phase):
        if phase not in ('create', 'runtime'):
            raise ValueError('invalid readiness phase')
        try:
            proof = self.verifier.verify(dict(binding['identity']), phase)
        except Exception:
            proof = None
        digest = hashlib.sha256(dumps(binding['engine'].policy).encode()).hexdigest()
        if (self.storage_failed or not isinstance(proof, NetworkProof)
                or proof.binding != binding_digest(binding['identity'])
                or proof.phase != phase or proof.policy_digest != digest
                or not self.clock() < proof.expires_at <= self.clock() + 30
                or not isinstance(proof.evidence_id, str) or not proof.evidence_id
                or type(proof.gateway_port) is not int
                or not 1024 <= proof.gateway_port <= 65535
                or proof.gateway_port != binding['gateway_port']
                or not binding['engine'].network_enabled):
            self._revoke(binding)
            return None
        return proof

    def _revoke(self, binding):
        lease = binding['lease']
        if lease:
            binding['ended'].add(lease['runID'])
        binding['lease'] = None
        binding['decisions'].clear()
        binding['engine'].network_decisions.clear()
        if lease:
            binding['engine'].revoke_all()

    def _lease(self, binding):
        lease = binding['lease']
        if lease and self.clock() >= lease['expires_at']:
            self._revoke(binding)
            lease = None
        return lease

    def check(self, value, phase):
        with self.lock:
            binding = self._binding(context(value))
            proof = self._proof(binding, phase)
            return {'ok': True, 'ready': bool(proof),
                    'reason': 'verified' if proof else 'enforcement_unavailable',
                    'detail': None if proof else getattr(self.verifier, 'last_failure', None)}

    def begin(self, value, renew=False):
        value = context(value)
        with self.lock:
            binding = self._binding(value)
            proof = self._proof(binding, 'runtime')
            if not proof:
                return {'ok': True, 'ready': False, 'reason': 'enforcement_unavailable'}
            source = self.claude_source if value.get('provider') == 'claude' else self.provider_source
            secret = binding['provider_secret'] if value.get('provider', 'codex') == 'codex' else None
            if not secret and not (source and source.available()):
                self._revoke(binding)
                return {'ok': True, 'ready': False, 'reason': 'provider_credential_unavailable'}
            lease = self._lease(binding)
            if lease and ((lease['runID'], lease['chatID']) != (value['runID'], value['chatID']) or lease.get('provider','codex') != value.get('provider','codex')):
                return {'ok': False, 'ready': False, 'reason': 'sandbox_busy'}
            if renew and not lease:
                return {'ok': False, 'ready': False, 'reason': 'lease_inactive'}
            if value['runID'] in binding['ended'] or len(binding['ended']) >= 10000:
                return {'ok': False, 'ready': False, 'reason': 'run_not_reusable'}
            # Durable audit precedes capability activation/extension.
            binding['engine'].audit.emit('sbx.run.renewed' if renew else 'sbx.run.started',
                sandbox_id=value['sandboxID'], project_id=value['projectID'],
                run_id=value['runID'], chat_id=value['chatID'])
            binding['lease'] = {'provider': value.get('provider','codex'), 'runID': value['runID'], 'chatID': value['chatID'],
                                'expires_at': self.clock() + LEASE_SECONDS}
            binding['last_begin'] = self.clock()
            gateway = 'http://host.docker.internal:' + str(proof.gateway_port)
            return {'ok': True, 'ready': True, 'leaseSeconds': LEASE_SECONDS,
                    'apiKeyPlaceholder': 'warden-proxy-managed',
                    'provider': value.get('provider','codex'),
                    **({'caCertificate': (binding['engine'].state/'gateway'/'ca'/'mitmproxy-ca-cert.pem').read_text()} if (binding['engine'].state/'gateway'/'ca'/'mitmproxy-ca-cert.pem').is_file() else {}),
                    'providerBaseURL': gateway + ('/anthropic' if value.get('provider') == 'claude' else '/openai/v1'), 'proxyURL': gateway,
                    **({'documentBaseURL': gateway + '/workspace/v1/documents'} if self.document_api else {})}

    def end(self, value):
        value = context(value)
        with self.lock:
            binding = self._binding(value)
            lease = self._lease(binding)
            if lease and (lease['runID'], lease['chatID']) != (value['runID'], value['chatID']):
                return {'ok': False, 'reason': 'run_mismatch'}
            self._revoke(binding)
            return {'ok': True, 'ready': False}

    def gateway(self, value):
        """Host launcher configuration. Never sent to a guest or frontend."""
        with self.lock:
            binding = self._binding(context(value))
            return {'ok': True, 'bindingID': value['sandboxID'],
                    'capability': binding['capability'], 'gatewayPort': binding['gateway_port']}

    def bind_gateway(self, value, port):
        with self.lock:
            binding = self._binding(context(value))
            if type(port) is not int or not 1024 <= port <= 65535:
                raise ValueError('invalid gateway port')
            if binding['gateway_port'] not in (None, port):
                raise ValueError('gateway binding immutable until restart')
            if any(other is not binding and other['gateway_port'] == port for other in self.bindings.values()):
                raise ValueError('gateway port already registered')
            self._set_gateway_port(binding, port)
            return {'ok': True, 'ready': False}

    def _set_gateway_port(self, binding, port):
        path = binding['engine'].state / 'gateway-port.json'
        temporary = path.with_suffix('.tmp')
        try:
            with temporary.open('w') as stream:
                temporary.chmod(0o600);stream.write(dumps(port));stream.flush();os.fsync(stream.fileno())
            os.replace(temporary, path)
            directory = os.open(path.parent, os.O_RDONLY)
            try: os.fsync(directory)
            finally: os.close(directory)
        except Exception:
            self.storage_failed = True
            raise
        binding['gateway_port'] = port

    def configure_provider(self, value, secret):
        with self.lock:
            binding = self._binding(context(value))
            if not isinstance(secret, str) or not 16 <= len(secret) <= 4096 or any(c.isspace() for c in secret):
                raise ValueError('invalid provider credential')
            self._revoke(binding)
            binding['engine'].redactor.register(secret)
            binding['engine'].audit.emit('credential.configured',
                credential={'provider': 'openai', 'storage': 'host_memory'})
            binding['provider_secret'] = secret
            return {'ok': True}

    def _provider_route(self, binding, path):
        if binding.get('lease', {}).get('provider') == 'claude':
            if not self.claude_source: raise ValueError('Claude not configured')
            return self.claude_source.route(path)
        if binding['provider_secret']:
            if path not in ('/v1/responses','/v1/chat/completions'): raise ValueError('unsupported provider route')
            return {'host': 'api.openai.com', 'path': path}
        if self.provider_source:
            return self.provider_source.route(path)
        raise ValueError('provider unavailable')

    def document_request(self, sandbox, capability, message):
        from .sbx_documents import MAX_BODY
        with self.lock:
            binding = self.bindings.get(sandbox)
            if (not binding or not isinstance(capability, str)
                    or not hmac.compare_digest(binding['capability'], capability)):
                raise ValueError('gateway authentication failed')
            if (not self.document_api or not self._proof(binding, 'runtime') or not self._lease(binding)):
                return {'allow': False, 'status': 503}
            if set(message) != {'action', 'method', 'path', 'body'}:
                return {'allow': False, 'status': 403}
            try:
                encoded_body = message['body']
                if not isinstance(encoded_body, str) or len(encoded_body) > (MAX_BODY + 2) // 3 * 4:
                    raise ValueError('invalid document body')
                body = base64.b64decode(encoded_body, validate=True)
                lease = binding['lease']
                trusted = {**binding['identity'], 'runID': lease['runID'], 'chatID': lease['chatID']}
                path, headers = self.document_api.prepare(message['method'], message['path'], body, trusted)
            except (ValueError, TypeError):
                return {'allow': False, 'status': 403}
            binding['engine'].audit.emit('sbx.document.authorized', sandbox_id=sandbox,
                project_id=trusted['projectID'], run_id=trusted['runID'], chat_id=trusted['chatID'],
                request={'method': message['method'], 'path': path.split('?')[0]})
        # Let End/revocation proceed while the local app responds. The app also
        # checks current run assignment before a write, and delivery rechecks it.
        try:
            result = self.document_api.dispatch(message['method'], path, body, headers)
        except Exception:
            return {'allow': False, 'status': 502}
        with self.lock:
            if self.bindings.get(sandbox) is not binding or not self._proof(binding, 'runtime'):
                return {'allow': False, 'status': 503}
            current = self._lease(binding)
            if (not current or current['runID'] != trusted['runID'] or current['chatID'] != trusted['chatID']):
                return {'allow': False, 'status': 503}
            binding['engine'].audit.emit('sbx.document.completed', sandbox_id=sandbox,
                project_id=trusted['projectID'], run_id=trusted['runID'], status=result['status'])
            return {'allow': True, **result}

    def proxy(self, sandbox, capability, message):
        if isinstance(message, dict) and message.get('action') == 'document':
            return self.document_request(sandbox, capability, message)
        with self.lock:
            binding = self.bindings.get(sandbox)
            if (not binding or not isinstance(capability, str)
                    or not hmac.compare_digest(binding['capability'], capability)):
                raise ValueError('gateway authentication failed')
            if not isinstance(message, dict):
                raise ValueError('invalid gateway message')
            action = message.get('action')
            if action not in ('authorize', 'egress', 'active', 'event', 'egress.finish', 'provider', 'providerRoute', 'destination'):
                raise ValueError('unsupported gateway action')
            if not self._proof(binding, 'runtime') or not self._lease(binding):
                return {'allow': False, 'active': False, 'status': 503,
                        'reason': 'active verified run required'}
            engine = binding['engine']
            sharing = getattr(self, 'sharing', None)
            lease = binding['lease']
            if sharing and action == 'authorize' and message.get('request',{}).get('host') in ('github.com','api.github.com'):
                try:
                    shared = sharing.github_grant(lease['chatID'], sandbox, engine, message['request'])
                    if shared:
                        grant, authorization = shared
                        if not self._lease(binding):
                            return {'allow':False,'status':403,'reason':'run expired during credential acquisition'}
                        decision = 'grepo-' + secrets.token_hex(16)
                        binding['decisions'][decision] = {'grant':grant,'chat':lease['chatID'],'run':lease['runID'],'expires':sharing.clock()+300}
                        engine.redactor.register(authorization.removeprefix('Bearer '))
                        engine.audit.emit('sbx.github.authorized',sandbox_id=sandbox,chat_id=lease['chatID'],run_id=lease['runID'],grant_id=grant)
                        return {'allow':True,'authorization':authorization,'decision_id':decision,'request_id':decision,'remaining_seconds':300,'expires_at':sharing.clock()+300}
                except Exception:
                    return {'allow':False,'status':403,'reason':'GitHub repository sharing unavailable'}
            if sharing and action == 'active' and str(message.get('decision_id','')).startswith('grepo-'):
                record=binding['decisions'].get(message['decision_id'])
                return {'active':bool(record and engine.network_enabled and record['expires']>sharing.clock() and record['run']==lease['runID'] and record['chat']==lease['chatID'] and sharing.github_active(record['grant'],lease['chatID'],sandbox))}
            if sharing and action == 'authorize' and message.get('request',{}).get('host') == 'docs.googleapis.com':
                try:
                    if not engine.network_enabled: raise ValueError('network is disabled')
                    grant, authorization = sharing.authorize(lease['chatID'], sandbox, message['request'])
                    if not self._lease(binding) or not sharing.active(grant['request_id'],lease['chatID'],sandbox): raise ValueError('grant or run expired')
                    decision = 'gdoc-' + secrets.token_hex(16)
                    binding['decisions'][decision] = {'grant':grant['request_id'],'chat':lease['chatID'],'run':lease['runID']}
                    engine.redactor.register(authorization.removeprefix('Bearer '))
                    engine.audit.emit('sbx.google.authorized',sandbox_id=sandbox,chat_id=lease['chatID'],run_id=lease['runID'],grant_id=grant['request_id'])
                    return {'allow':True,'authorization':authorization,'decision_id':decision,'request_id':decision,'expires_at':grant['expires_at'],'remaining_seconds':max(0,grant['expires_at']-self.sharing.clock())}
                except (ValueError, OSError):
                    return {'allow':False,'status':403,'reason':'Google document access requires an active sharing grant'}
            if sharing and action == 'active' and str(message.get('decision_id','')).startswith('gdoc-'):
                record=binding['decisions'].get(message['decision_id'])
                return {'active':bool(record and engine.network_enabled and record['run']==lease['runID'] and record['chat']==lease['chatID'] and sharing.active(record['grant'],lease['chatID'],sandbox))}
            if action == 'providerRoute':
                try:
                    return {'allow': True, **self._provider_route(binding, message.get('path'))}
                except (ValueError, TypeError):
                    return {'allow': False, 'reason': 'unsupported provider route'}
            if action == 'destination':
                # This happens before any gateway DNS lookup. Restrict even
                # CONNECT hostnames so rejected names cannot become DNS egress.
                from .egress import permits
                request = message.get('request', {})
                host = request.get('host')
                allow = host in ('github.com', 'api.github.com', 'api.figma.com', 'docs.googleapis.com') or permits(
                    engine.policy['egress'], host, request.get('method'), request.get('scheme'))
                if allow:
                    engine.audit.emit('dns.query', hostname=host, reason='SBX destination checked before resolution')
                return {'allow': allow}
            if action == 'provider':
                record = binding['decisions'].get(message.get('decision_id'))
                request = message.get('request')
                routes = []
                for path in PROVIDER_ROUTES:
                    try: routes.append(self._provider_route(binding, path))
                    except ValueError: pass
                if (not record or record != request or not engine.active(message['decision_id'])
                        or record.get('scheme') != 'https' or record.get('method') != 'POST'
                        or {'host': record.get('host'), 'path': record.get('path')} not in routes):
                    return {'allow': False, 'status': 403, 'reason': 'provider credential denied'}
                source = self.claude_source if binding['lease'].get('provider') == 'claude' else self.provider_source
                headers = (source.headers() if binding['lease'].get('provider') == 'claude' else
                           ({'Authorization': 'Bearer ' + binding['provider_secret']} if binding['provider_secret'] else source.headers()))
                if (set(headers) - {'Authorization', 'ChatGPT-Account-ID'}
                        or not headers.get('Authorization', '').startswith('Bearer ')
                        or any(not isinstance(v, str) or '\r' in v or '\n' in v for v in headers.values())):
                    raise ValueError('invalid credential headers')
                for value in headers.values():
                    engine.redactor.register(value)
                    engine.redactor.register(value.removeprefix('Bearer '))
                engine.audit.emit('sbx.provider.authorized',
                    sandbox_id=sandbox, run_id=binding['lease']['runID'],
                    decision_id=message['decision_id'], hostname=record['host'])
                return {'allow': True, 'headers': headers, 'authorization': headers['Authorization']}
            if action == 'egress':
                request = message.get('request')
                if not isinstance(request, dict) or request.get('tls'):
                    return {'allow': False, 'status': 403, 'reason': 'opaque TLS unsupported'}
                result = internal(engine, message)
                if result.get('allow'):
                    binding['decisions'][result['decision_id']] = dict(request)
                return result
            if action == 'egress.finish':
                binding['decisions'].pop(message.get('decision_id'), None)
            return internal(engine, message)

    def dispatch(self, message):
        if self.storage_failed:
            raise ValueError('durable registry unavailable')
        if not isinstance(message, dict) or type(message.get('version')) is not int or message['version'] != 1:
            raise ValueError('unsupported protocol')
        operation = message.get('operation')
        if operation == 'sharing':
            if set(message) != {'version','operation','action','data'} or not getattr(self,'sharing',None):
                raise ValueError('sharing unavailable')
            return {'version':1,'ok':True,'result':self.sharing.dispatch(message['action'],message['data'])}
        fields = {'version', 'operation', 'context'}
        if operation == 'check': fields.add('phase')
        if operation == 'configureProvider': fields.add('secret')
        if operation == 'bindGateway': fields.add('port')
        if operation == 'proxy': fields = {'version', 'operation', 'bindingID', 'capability', 'message'}
        if set(message) != fields:
            raise ValueError('unexpected protocol fields')
        if operation == 'proxy': result = self.proxy(message['bindingID'], message['capability'], message['message'])
        elif operation == 'register': result = self.register(message['context'])
        elif operation == 'check': result = self.check(message['context'], message['phase'])
        elif operation in ('begin', 'renew'): result = self.begin(message['context'], renew=operation == 'renew')
        elif operation == 'end': result = self.end(message['context'])
        elif operation == 'gateway': result = self.gateway(message['context'])
        elif operation == 'configureProvider': result = self.configure_provider(message['context'], message['secret'])
        elif operation == 'bindGateway': result = self.bind_gateway(message['context'], message['port'])
        else: raise ValueError('unknown operation')
        return {'version': 1, **result}

    def close(self):
        stop_refresher = getattr(self.verifier, 'stop_refresher', None)
        if stop_refresher:
            stop_refresher()
        gateway_pool = getattr(self, 'gateway_pool', None)
        if gateway_pool:
            gateway_pool.close()
        for binding in self.bindings.values():
            binding['provider_secret'] = None
            binding['engine'].db.close()
            os.close(binding['engine'].audit.fd)


class Server(LimitedThreads, socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


def handler(registry):
    class Handler(socketserver.StreamRequestHandler):
        def handle(self):
            self.connection.settimeout(10)
            try:
                line = self.rfile.readline(MAX_MESSAGE + 1)
                if len(line) > MAX_MESSAGE or not line.endswith(b'\n'):
                    raise ValueError('invalid frame')
                response = registry.dispatch(strict_json(line))
            except Exception:
                response = {'version': 1, 'ok': False, 'ready': False, 'allow': False,
                            'reason': 'control_request_rejected'}
            self.wfile.write((dumps(response) + '\n').encode())
    return Handler


def rpc(path, message):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.settimeout(30)
        conn.connect(str(path))
        conn.sendall((dumps(message) + '\n').encode())
        with conn.makefile('rb') as stream:
            data = stream.readline(MAX_MESSAGE + 1)
        if len(data) > MAX_MESSAGE or not data.endswith(b'\n'):
            raise OSError('invalid control response')
        value = strict_json(data)
        if not isinstance(value, dict) or value.get('version') != 1 or value.get('ok') is False:
            raise OSError('control request rejected')
        return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--sbx', help='explicit trusted SBX executable; enables pinned CLI verifier')
    parser.add_argument('--mitmdump', help='explicit pinned mitmdump executable for managed gateways')
    parser.add_argument('--manage-network', action='store_true', help='manage only registered sandbox gateway rules')
    parser.add_argument('--google-config', type=Path, help='private Google OAuth client configuration for file sharing')
    parser.add_argument('--claude-auth-file', type=Path, help='private host Claude OAuth credential file')
    parser.add_argument('--codex-auth-file', type=Path, help='private host Codex login, read without copying or refreshing')
    parser.add_argument('--document-api-origin', help='fixed local app HTTP origin, e.g. http://127.0.0.1:8080')
    parser.add_argument('--document-api-key-file', type=Path, help='private app/Warden shared signing key; never sent to sandbox')
    args = parser.parse_args()
    if bool(args.document_api_origin) != bool(args.document_api_key_file):
        parser.error('document API origin and key file must be configured together')
    os.umask(0o077)
    args.state.mkdir(parents=True, mode=0o700, exist_ok=True)
    info = args.state.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid():
        raise SystemExit('state must be an owned private directory')
    args.state.chmod(0o700)
    with (args.state / 'service.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        path = args.state / 'sbx-control.sock'
        if len(str(path).encode()) > 100:
            raise SystemExit('state path too long for private Unix socket')
        path.unlink(missing_ok=True)
        document_api = None
        if args.document_api_origin:
            from .sbx_documents import DocumentAPI
            document_api = DocumentAPI(args.document_api_origin, args.document_api_key_file)
        registry = Registry(args.state, document_api=document_api)
        from .sharing import Sharing, GoogleConnection
        from .github_app import from_config
        from .core import Redactor
        github_config = os.environ.get('WARDEN_GITHUB_APP_BROKER')
        registry.sharing = Sharing(args.state, github=from_config(github_config, Redactor()) if github_config else None)
        registry.sharing.google = GoogleConnection(args.state, args.google_config)
        if args.claude_auth_file:
            from .sbx_credentials import ClaudeCredentials
            registry.claude_source = ClaudeCredentials(args.claude_auth_file)
        if args.codex_auth_file:
            from .sbx_credentials import CodexCredentials
            registry.provider_source = CodexCredentials(args.codex_auth_file)
        if args.sbx or args.mitmdump or args.manage_network:
            if not args.sbx or not args.mitmdump:
                raise SystemExit('managed mode requires --sbx and --mitmdump')
            from .sbx_verifier import SbxCliVerifier, GatewayPool
            registry.gateway_pool = GatewayPool(registry, args.mitmdump, path)
            registry.verifier = SbxCliVerifier(registry, args.sbx, args.manage_network)
            registry.verifier.start_refresher()
        try:
            with Server(str(path), handler(registry)) as server:
                path.chmod(0o600)
                signal.signal(signal.SIGTERM, lambda *_: threading.Thread(target=server.shutdown, daemon=True).start())
                print('SBX control ready; readiness requires verified gateway policy' if args.sbx else 'SBX control ready; runtime enforcement unsupported (fail closed)', flush=True)
                server.serve_forever()
        finally:
            path.unlink(missing_ok=True)
            registry.close()


if __name__ == '__main__':
    main()
