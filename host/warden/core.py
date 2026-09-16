from __future__ import annotations

import base64
import hashlib
import hmac
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import sqlite3
import threading
import time
import uuid
from datetime import datetime, timezone
from urllib.parse import urlsplit, parse_qsl, unquote
from .retention import without_bodies, scrub_requests
from .clipboard import ClipboardTransfer

ROOT = Path(__file__).resolve().parents[2]
GITHUB_SUFFIXES = ('github.com', 'githubusercontent.com', 'githubassets.com', 'github.io', 'githubapp.com', 'github.dev', 'githubpreview.dev', 'ghcr.io', 'githubstatus.com', 'github.blog', 'githubcopilot.com', 'copilot.github.com')
SENSITIVE = re.compile(r'authorization|cookie|password|passwd|secret|token|api.?key|credential|private.?key|signature|^sig$|^key$', re.I)
TOKEN_PATTERN = re.compile(r'(?:gh[pousr]_[A-Za-z0-9_]{15,}|github_pat_[A-Za-z0-9_]{15,}|sk-[A-Za-z0-9_-]{15,}|(?:Bearer|Basic)\s+[A-Za-z0-9._~+/=-]+|-----BEGIN [^-]*PRIVATE KEY-----[\s\S]*?-----END [^-]*PRIVATE KEY-----)', re.I)
SAFE_HEADERS = {'accept', 'content-type', 'content-length', 'user-agent', 'x-github-api-version', 'host', 'date', 'server', 'x-github-request-id', 'etag', 'last-modified', 'cache-control', 'content-encoding', 'connection'}

def dumps(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=True, allow_nan=False)

def strict_json(text):
    def pairs(items):
        obj = {}
        for k, v in items:
            if k in obj: raise ValueError('duplicate JSON key')
            obj[k] = v
        return obj
    return json.loads(text, object_pairs_hook=pairs, parse_constant=lambda _: (_ for _ in ()).throw(ValueError('nonfinite JSON')))

def github_host(host):
    host = host.lower().rstrip('.')
    return any(host == suffix or host.endswith('.' + suffix) for suffix in GITHUB_SUFFIXES)

class Redactor:
    def __init__(self): self.secrets = set()
    def register(self, value):
        if value:
            self.secrets.update([value, base64.b64encode(value.encode()).decode()])
    def text(self, value):
        for secret in sorted(self.secrets, key=len, reverse=True): value = value.replace(secret, '[REDACTED]')
        return TOKEN_PATTERN.sub('[REDACTED]', value)
    def clean(self, value):
        if isinstance(value, dict): return {self.text(str(k)): '[REDACTED]' if SENSITIVE.search(str(k)) and k not in ('token_configured', 'credential') else self.clean(v) for k,v in value.items()}
        if isinstance(value, list): return [self.clean(v) for v in value]
        if isinstance(value, str): return self.text(value)
        return value
    def headers(self, pairs):
        return [[k, self.text(v) if k.lower() in SAFE_HEADERS else '[REDACTED]'] for k,v in pairs]
    def body(self, body, content_type=''):
        # Bodies remain available transiently for enforcement, never persistence.
        return {'bytes': len(body), 'capture': 'omitted_policy'}

class Audit:
    def __init__(self, path, redactor):
        self.path, self.redactor = Path(path), redactor
        self.lock = threading.RLock(); self.instance = str(uuid.uuid4()); self.seq = 0
        self.previous = '0'*64
        self.path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.fd = os.open(self.path, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
    def emit(self, event_type, severity='info', **fields):
        with self.lock:
            event = {'schema_version': '1.0.0', 'event_id': str(uuid.uuid4()), 'time': datetime.now(timezone.utc).isoformat(timespec='milliseconds').replace('+00:00', 'Z'), 'event_type': event_type, 'severity': severity, 'producer': {'name': 'warden', 'version': '0.1.0', 'instance_id': self.instance}, 'sequence': self.seq+1, 'previous_hash': self.previous, **self.redactor.clean(without_bodies(fields))}
            event['event_hash'] = hashlib.sha256(dumps(event).encode()).hexdigest()
            payload = (dumps(event)+'\n').encode()
            # Audit is synchronous: no permit is returned before durable logging.
            while payload:
                count = os.write(self.fd, payload)
                if count <= 0: raise OSError('audit write failed')
                payload = payload[count:]
            os.fsync(self.fd)
            self.seq += 1; self.previous = event['event_hash']
            return event

class Operations:
    def __init__(self, path=None):
        document = json.loads(Path(path or ROOT/'vendor/github-operations.json').read_text())
        self.revision = document['revision']; self.routes = []
        for item in document['operations']:
            parts = re.split(r'(\{[^{}]+\})', item['path'])
            pattern = ''.join('(?P<'+re.sub(r'\W','_',part[1:-1])+'>[^/]+)' if part.startswith('{') else re.escape(part) for part in parts)
            self.routes.append((item, re.compile('^'+pattern+'$')))
        # Static paths win over parameters, matching the API's routing model.
        self.routes.sort(key=lambda entry: entry[0]['path'].count('{'))
    def match(self, method, path):
        for op, pattern in self.routes:
            if op['method'] == method:
                match = pattern.fullmatch(path)
                if match: return op, match.groupdict()
        return None, {}

def validate_policy(value):
    required={'version', 'max_grant_seconds', 'deny_operations', 'deny_repositories', 'max_request_bytes'}
    if not isinstance(value, dict) or not required<=set(value) or set(value)-required-{'egress','allowed_repositories','allowed_figma_files','allowed_google_documents'}: raise ValueError('policy has missing or unknown fields')
    if value['version'] != 1: raise ValueError('unsupported policy version')
    for name, maximum in [('max_grant_seconds', 86400), ('max_request_bytes', 8388608)]:
        if type(value[name]) is not int or not 1 <= value[name] <= maximum: raise ValueError('invalid '+name)
    for name in ['deny_operations', 'deny_repositories']:
        if not isinstance(value[name], list) or any(not isinstance(v, str) for v in value[name]): raise ValueError('invalid '+name)
    from . import egress
    if 'egress' in value:egress.validate(value['egress'])
    if 'allowed_repositories' in value and (not isinstance(value['allowed_repositories'],list) or any(not isinstance(r,str) or not egress.REPO.fullmatch(r) for r in value['allowed_repositories'])):raise ValueError('invalid repository allowlist')
    if 'allowed_figma_files' in value:
        from .figma import FILE_KEY
        if not isinstance(value['allowed_figma_files'], list) or any(not isinstance(k, str) or not FILE_KEY.fullmatch(k) for k in value['allowed_figma_files']):
            raise ValueError('invalid Figma file allowlist')
    if 'allowed_google_documents' in value:
        from .google_docs import DOCUMENT_ID
        if not isinstance(value['allowed_google_documents'], list) or any(not isinstance(k, str) or not DOCUMENT_ID.fullmatch(k) for k in value['allowed_google_documents']):
            raise ValueError('invalid Google document allowlist')
    return value

class Engine:
    def __init__(self, state, operations=None, clock=time.time, monotonic=time.monotonic):
        self.state = Path(state); self.state.mkdir(parents=True, exist_ok=True, mode=0o700)
        os.chmod(self.state, 0o700)
        self.clock, self.monotonic = clock, monotonic
        self.started_wall, self.started_mono = clock(), monotonic()
        self.lock = threading.RLock(); self.redactor = Redactor(); self.token = None
        from .figma import Connection
        self.figma = Connection(self.redactor, clock=self.now)
        from .google_docs import Connection as GoogleDocsConnection
        self.google_docs = GoogleDocsConnection(self.redactor, clock=self.now)
        self.github_app = None
        if os.environ.get('WARDEN_GITHUB_APP_BROKER'):
            from .github_app import from_config
            self.github_app = from_config(os.environ['WARDEN_GITHUB_APP_BROKER'], self.redactor, self.now)
        self.git_reviews = {}  # Bounded, short-lived source previews; never SQLite/audit.
        self.network_decisions = {}
        self.network_enabled = not (self.state/'network-disconnected').exists()
        self.rest_reviews = {}
        self.clipboard = ClipboardTransfer()
        self.audit = Audit(self.state/'audit/events.jsonl', self.redactor)
        self.operations = operations or Operations()
        self.fingerprint_key = secrets.token_bytes(32)
        self.policy_path = self.state/'policy.json'
        if not self.policy_path.exists(): self.policy_path.write_text((ROOT/'config/policy.template.json').read_text()); os.chmod(self.policy_path, 0o600)
        self.policy = validate_policy(strict_json(self.policy_path.read_text()))
        self.db = sqlite3.connect(self.state/'control.sqlite', check_same_thread=False)
        self.db.row_factory = sqlite3.Row
        self.db.executescript('''PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
        CREATE TABLE IF NOT EXISTS requests(id TEXT PRIMARY KEY, fingerprint TEXT, operation TEXT, path TEXT, repository TEXT, summary TEXT, status TEXT, created REAL);
        CREATE TABLE IF NOT EXISTS grants(id TEXT PRIMARY KEY, request_id TEXT, kind TEXT, fingerprint TEXT, operation TEXT, path TEXT, predicates TEXT, expires REAL, remaining INTEGER, revoked INTEGER DEFAULT 0);
        CREATE TABLE IF NOT EXISTS decisions(id TEXT PRIMARY KEY, grant_id TEXT, expires REAL);
        ''')
        self.db.execute('CREATE INDEX IF NOT EXISTS requests_created_id ON requests(created DESC,id DESC)')
        self.db.execute('CREATE INDEX IF NOT EXISTS requests_status_created_id ON requests(status,created DESC,id DESC)')
        scrub_requests(self.db)
        # Authorization never survives a control-plane restart or clock reset.
        self.db.execute('UPDATE grants SET revoked=1'); self.db.execute("UPDATE requests SET status='stale' WHERE status='pending'"); self.db.commit()
        self.audit.emit('system.started', operation_catalog_revision=self.operations.revision)
    def now(self):
        return max(self.clock(), self.started_wall+self.monotonic()-self.started_mono)
    def set_token(self, token):
        if self.github_app: raise ValueError('GitHub App broker manages credentials; manual tokens are disabled')
        if not isinstance(token, str) or not re.fullmatch(r'(?:github_pat_|ghp_|gho_)[A-Za-z0-9_]{20,}', token): raise ValueError('expected a GitHub personal access or OAuth token')
        with self.lock:
            self.redactor.register(token); self.token = token
            self.audit.emit('credential.configured', credential={'provider': 'github', 'storage': 'host_memory'})
    def configure_figma(self, client_id, client_secret, redirect_uri):
        with self.lock:
            self.figma.configure(client_id, client_secret, redirect_uri)
            self._revoke_figma()
            self.audit.emit('credential.configured', credential={'provider':'figma','storage':'host_memory'})
    def _revoke_figma(self):
        with self.db:
            self.db.execute("UPDATE grants SET revoked=1 WHERE operation LIKE 'figma/%'")
            self.db.execute("UPDATE requests SET status='stale' WHERE operation LIKE 'figma/%' AND status='pending'")
    def connect_figma(self):
        with self.lock:
            self.audit.emit('credential.connection_started', credential={'provider':'figma'})
            return {'authorization_url':self.figma.start()}
    def complete_figma(self, state, code):
        with self.lock:
            self.figma.complete(state, code)
            try:
                self._revoke_figma()
                self.audit.emit('credential.connected', credential={'provider':'figma','storage':'host_memory'})
            except Exception:
                self.figma.disconnect()
                raise
    def disconnect_figma(self):
        with self.lock:
            self.figma.disconnect()
            self._revoke_figma()
            self.audit.emit('credential.disconnected', credential={'provider':'figma'})
    def configure_google_docs(self, client_id, client_secret, redirect_uri):
        with self.lock:
            self.google_docs.configure(client_id, client_secret, redirect_uri)
            self._revoke_google_docs()
            self.audit.emit('credential.configured', credential={'provider':'google_docs','storage':'host_memory'})
    def _revoke_google_docs(self):
        with self.db:
            self.db.execute("UPDATE grants SET revoked=1 WHERE operation LIKE 'google_docs/%'")
            self.db.execute("UPDATE requests SET status='stale' WHERE operation LIKE 'google_docs/%' AND status='pending'")
    def connect_google_docs(self):
        with self.lock:
            self.audit.emit('credential.connection_started', credential={'provider':'google_docs'})
            return {'authorization_url':self.google_docs.start()}
    def complete_google_docs(self, state, code):
        with self.lock:
            self.google_docs.complete(state, code)
            try:
                self._revoke_google_docs()
                self.audit.emit('credential.connected', credential={'provider':'google_docs','storage':'host_memory'})
            except Exception:
                self.google_docs.disconnect()
                raise
    def disconnect_google_docs(self):
        with self.lock:
            self.google_docs.disconnect()
            self._revoke_google_docs()
            self.audit.emit('credential.disconnected', credential={'provider':'google_docs'})
    def save_policy(self, policy):
        validate_policy(policy)
        with self.lock:
            self.audit.emit('policy.updated', policy=policy)
            tmp = self.policy_path.with_suffix('.tmp'); tmp.write_text(dumps(policy)+'\n'); os.chmod(tmp, 0o600); os.replace(tmp, self.policy_path)
            self.policy = policy
            self.network_decisions.clear()
            self.db.execute('UPDATE grants SET revoked=1'); self.db.commit()
    def revoke_all(self):
        with self.lock:
            self.audit.emit('approval.revoked_all', actor={'type':'human'})
            self.db.execute('UPDATE grants SET revoked=1');self.db.commit()
            self.network_decisions.clear()
    def set_network(self, enabled):
        if type(enabled) is not bool:raise ValueError('enabled must be boolean')
        with self.lock:
            # Persist the deny state before acknowledging it. Reconnection never
            # restores permissions that were revoked by disconnecting.
            flag=self.state/'network-disconnected'
            if not enabled:flag.write_text('disconnected\n');self.network_enabled=False
            else:flag.unlink(missing_ok=True);self.network_enabled=True
            self.revoke_all()
            self.audit.emit('network.connected' if enabled else 'network.disconnected')
    def authorize_egress(self, request):
        from .egress import permits
        with self.lock:
            host=request['host'];method=request['method'];scheme=request.get('scheme','https');tls=request.get('tls',False)
            policy=self.policy.get('egress',{'mode':'public','destinations':[]})
            allowed=self.network_enabled and permits(policy,host,method,scheme,tls)
            self.audit.emit('egress.allowed' if allowed else 'egress.denied',hostname=host,request={'method':method,'host':host,'scheme':scheme},reason='destination policy')
            if not allowed:return {'allow':False,'reason':'network disconnected or destination not allowed','status':403}
            now=self.now();self.network_decisions={k:v for k,v in self.network_decisions.items() if v>now}
            if len(self.network_decisions)>=4096:raise ValueError('too many active network requests')
            decision=str(uuid.uuid4());self.network_decisions[decision]=now+3600
            return {'allow':True,'decision_id':decision,'remaining_seconds':3600,'request_id':str(uuid.uuid4())}
    def normalize(self, request):
        method = request['method']; host = request['host']; path = request['path']; scheme = request.get('scheme', 'https'); port = request.get('port', 443)
        if not isinstance(method, str) or not re.fullmatch('[A-Z]+', method): raise ValueError('invalid method')
        if not isinstance(host, str) or not re.fullmatch(r'[a-z0-9.-]+', host): raise ValueError('noncanonical host')
        if not isinstance(path, str) or not path.startswith('/') or path.startswith('//') or len(path) > 16384 or any(ord(c) < 33 or ord(c) > 126 for c in path): raise ValueError('invalid request target')
        if '#' in path or '\\' in path: raise ValueError('ambiguous request target')
        parts = urlsplit(path)
        # Encoded path delimiters/dot segments can change GitHub routing semantics.
        if '%' in parts.path or any(segment in ('.', '..') for segment in parts.path.split('/')): raise ValueError('encoded or ambiguous API path unsupported')
        query = parse_qsl(parts.query, keep_blank_values=True, strict_parsing=False)
        if any(SENSITIVE.search(k) for k,v in query): raise ValueError('credentials in query are forbidden')
        if len({k for k,v in query}) != len(query): raise ValueError('duplicate query parameters unsupported')
        body = base64.b64decode(request.get('body_base64', ''), validate=True)
        if len(body) > self.policy['max_request_bytes']: raise ValueError('request body too large')
        headers = request.get('headers', [])
        if not isinstance(headers, list) or len(headers) > 100: raise ValueError('invalid headers')
        seen = set()
        for pair in headers:
            if not isinstance(pair, list) or len(pair) != 2 or not all(isinstance(v, str) for v in pair): raise ValueError('invalid header')
            k,v = pair; k = k.lower()
            if not re.fullmatch(r'[a-z0-9!#$%&\x27*+.^_`|~-]+', k) or any(ord(c)<32 or ord(c)>126 for c in v): raise ValueError('invalid header')
            if k in seen: raise ValueError('duplicate headers unsupported')
            seen.add(k)
            if k in ('authorization', 'proxy-authorization', 'cookie', 'host', 'content-length', 'transfer-encoding', 'connection', 'x-figma-token', 'x-goog-api-key', 'x-goog-user-project'): raise ValueError('proxy must strip transport and credential headers')
        normalized = {'method': method, 'host': host, 'path': path, 'scheme': scheme, 'port': port, 'headers': sorted([[k.lower(),v] for k,v in headers]), 'body_base64': request.get('body_base64','')}
        fingerprint = hmac.new(self.fingerprint_key, dumps(normalized).encode(), hashlib.sha256).hexdigest()
        if host == 'github.com' and scheme == 'https' and port == 443:
            from .git_protocol import inspect
            git = inspect(method, path, headers, body)
            repository = git['repository']
            operation = 'git/push' if git['write'] else 'git/read'
            summary = {'method':method, 'host':host, 'path':path, 'operation':operation,
                       'repository':repository, 'body':self.redactor.body(body)}
            normalized['path'] = '/'+repository+'.git'
            if git['write']:
                summary['update'] = git['update']
                review = request.get('git_review')
                if not isinstance(review, dict) or review.get('update') != git['update'] or review.get('pack_request_sha256') != hashlib.sha256(body).hexdigest():
                    raise ValueError('verified Git review is required before requesting push permission')
                if set(review) != {'update','pack_request_sha256','base','patch','stat','commits','truncated'} or any(not isinstance(review[k],str) or len(review[k])>262144 for k in ('patch','stat','commits','base')) or type(review['truncated']) is not bool:
                    raise ValueError('invalid Git review')
                if review['truncated']: raise ValueError('push review exceeds limit; split the change into smaller pushes')
                self.git_reviews = {k:v for k,v in self.git_reviews.items() if v[0]>self.now()}
                if fingerprint not in self.git_reviews and len(self.git_reviews)>=64:
                    self.git_reviews.pop(next(iter(self.git_reviews)))
                self.git_reviews[fingerprint] = (self.now()+600, review)
            else:
                # Discovery for both services and upload-pack share one explicitly
                # approved repository read session. receive-pack never matches it.
                fingerprint = hmac.new(self.fingerprint_key, ('git/read:'+repository).encode(), hashlib.sha256).hexdigest()
            return normalized, fingerprint, {'operation_id':operation}, repository, None, summary
        if host == 'api.figma.com':
            from .figma import operation
            op, file_key = operation(method, parts.path, query, body)
            summary = {'method':method, 'host':host, 'path':self.redactor.text(path),
                       'operation':op['operation_id'], 'file_key':file_key, 'provider':'figma',
                       'body':self.redactor.body(body)}
            return normalized, fingerprint, op, '', None, summary
        if host == 'docs.googleapis.com':
            from .google_docs import operation
            op, document_id = operation(method, parts.path, query, body)
            summary = {'method':method, 'host':host, 'path':self.redactor.text(path),
                       'operation':op['operation_id'], 'document_id':document_id, 'provider':'google_docs',
                       'body':self.redactor.body(body)}
            return normalized, fingerprint, op, '', None, summary
        op, params = self.operations.match(method, parts.path)
        repository = (params.get('owner','')+'/'+params.get('repo','')).strip('/')
        body_json = None
        if body:
            if not any(k.lower() == 'content-type' and v.split(';')[0].strip().lower() == 'application/json' for k,v in headers): raise ValueError('only JSON API request bodies are supported')
            body_json = strict_json(body.decode())
            if not isinstance(body_json, dict): raise ValueError('JSON request body must be an object')
        summary = {'method': method, 'host': host, 'path': self.redactor.text(path), 'headers': self.redactor.headers(headers), 'body': self.redactor.body(body), 'operation': op['operation_id'] if op else None, 'repository': repository}
        if op and op['operation_id']=='pulls/create' and host=='api.github.com' and body_json:
            if len(body)>65536: raise ValueError('pull request exceeds review limit')
            self.rest_reviews={k:v for k,v in self.rest_reviews.items() if v[0]>self.now()}
            if fingerprint not in self.rest_reviews and len(self.rest_reviews)>=64: self.rest_reviews.pop(next(iter(self.rest_reviews)))
            self.rest_reviews[fingerprint]=(self.now()+600,body_json)
        return normalized, fingerprint, op, repository, body_json, summary
    def authorize(self, request):
        with self.lock:
            request_id = str(uuid.uuid4())
            try: normalized, fingerprint, op, repository, body_json, summary = self.normalize(request)
            except (ValueError, TypeError, KeyError, UnicodeError) as e:
                self.audit.emit('request.denied', severity='warning', request_id=request_id, reason=str(e))
                return {'allow': False, 'status': 403, 'reason': str(e), 'request_id': request_id}
            self.audit.emit('http.request', request_id=request_id, request=summary)
            reason = None
            is_google_docs = normalized['host'] == 'docs.googleapis.com'
            is_figma = normalized['host'] == 'api.figma.com'
            is_git = op and op['operation_id'] in ('git/read','git/push') and normalized['host']=='github.com'
            if not self.network_enabled:reason='network disconnected'
            elif is_figma and 'allowed_figma_files' in self.policy and summary.get('file_key') and summary['file_key'] not in self.policy['allowed_figma_files']: reason='Figma file outside the allowed files'
            elif is_google_docs and 'allowed_google_documents' in self.policy and summary['document_id'] not in self.policy['allowed_google_documents']: reason='Google document outside the allowed documents'
            elif not (is_figma or is_google_docs) and 'allowed_repositories' in self.policy and repository.lower() not in [r.lower() for r in self.policy['allowed_repositories']]:reason='repository outside this project'
            elif not is_git and (normalized['host'] not in ('api.github.com', 'api.figma.com', 'docs.googleapis.com') or normalized['scheme'] != 'https' or normalized['port'] != 443): reason = 'unsupported provider channel; use approved HTTPS REST or Git smart HTTP'
            elif self.github_app and not (is_figma or is_google_docs) and (not repository or repository.split('/')[0].lower()!=self.github_app.owner.lower()): reason='repository outside configured GitHub App owner'
            elif not op: reason = 'operation absent from pinned REST catalog'
            elif op['operation_id'] in self.policy['deny_operations'] or repository.lower() in [r.lower() for r in self.policy['deny_repositories']]: reason = 'denied by local policy'
            if not reason and self.github_app and not (is_figma or is_google_docs):
                from .github_app import permissions
                try: permissions(op['operation_id'])
                except ValueError: reason='operation is not supported by the GitHub App broker'
            if reason:
                self.audit.emit('request.denied', severity='warning', request_id=request_id, reason=reason)
                return {'allow': False, 'status': 403, 'reason': reason, 'request_id': request_id}
            now = self.now()
            candidates = self.db.execute('SELECT * FROM grants WHERE revoked=0 AND expires>? AND remaining!=0 ORDER BY CASE kind WHEN \'exact\' THEN 0 ELSE 1 END', (now,)).fetchall()
            for grant in candidates:
                if grant['kind'] == 'exact': match = hmac.compare_digest(grant['fingerprint'], fingerprint)
                else:
                    match = op['operation_id'] != 'git/push' and grant['operation'] == op['operation_id'] and grant['path'] == normalized['path']
                    for pointer, expected in json.loads(grant['predicates']).items():
                        current = body_json
                        for part in pointer.strip('/').split('/'):
                            part = part.replace('~1','/').replace('~0','~')
                            if isinstance(current, dict) and part in current: current = current[part]
                            else: match = False; break
                        if dumps(current) != dumps(expected): match = False
                if not match: continue
                try:
                    authorization = self.google_docs.authorization() if is_google_docs else self.figma.authorization() if is_figma else self.github_app.authorization(repository, op['operation_id']) if self.github_app else ('Bearer ' + self.token if self.token else None)
                    if not authorization: raise ValueError('missing credential')
                except Exception:
                    provider = 'Google Docs' if is_google_docs else 'Figma' if is_figma else 'GitHub'
                    self.audit.emit('request.denied', request_id=request_id, reason='host '+provider+' credential unavailable')
                    return {'allow':False, 'status':503, 'reason':'connect '+provider+' on the host', 'request_id':request_id}
                now = self.now()
                if grant['expires'] <= now: continue
                decision_id = str(uuid.uuid4())
                self.audit.emit('request.allowed', request_id=request_id, decision_id=decision_id, grant_id=grant['id'], expires_at=grant['expires'], operation=op['operation_id'])
                with self.db:
                    if grant['remaining'] > 0: self.db.execute('UPDATE grants SET remaining=remaining-1 WHERE id=?', (grant['id'],))
                    self.db.execute('INSERT INTO decisions VALUES(?,?,?)', (decision_id, grant['id'], grant['expires']))
                    self.db.execute("UPDATE requests SET status='executed' WHERE fingerprint=? AND status='pending'", (fingerprint,))
                return {'allow': True, 'request_id': request_id, 'decision_id': decision_id, 'grant_id': grant['id'], 'expires_at': grant['expires'], 'remaining_seconds': grant['expires']-now, 'authorization': authorization}
            existing = self.db.execute("SELECT id FROM requests WHERE fingerprint=? AND status='pending'", (fingerprint,)).fetchone()
            if existing: request_id = existing['id']
            else:
                count = self.db.execute("SELECT count(*) FROM requests WHERE status='pending'").fetchone()[0]
                if count >= 1000: return {'allow': False, 'status': 429, 'reason': 'approval queue full', 'request_id': request_id}
                with self.db: self.db.execute('INSERT INTO requests VALUES(?,?,?,?,?,?,?,?)', (request_id, fingerprint, op['operation_id'], normalized['path'], repository, dumps(summary), 'pending', now))
                self.audit.emit('approval.requested', request_id=request_id, operation=op['operation_id'], request=summary)
            return {'allow': False, 'status': 428, 'reason': 'human approval required; approve on host, then retry this exact request', 'request_id': request_id}
    def approve(self, request_id, kind, ttl, predicates=None):
        if kind not in ('exact', 'scoped'): raise ValueError('invalid grant kind')
        if type(ttl) is not int or not 1 <= ttl <= self.policy['max_grant_seconds']: raise ValueError('invalid duration')
        predicates = predicates or {}
        if not isinstance(predicates, dict) or len(predicates)>100 or any(not re.fullmatch(r'(?:/(?:[^~/]|~[01])*)+', key) for key in predicates): raise ValueError('predicates must map JSON pointers to exact values')
        with self.lock:
            row = self.db.execute("SELECT * FROM requests WHERE id=? AND status='pending'", (request_id,)).fetchone()
            if not row: raise ValueError('pending request not found')
            if row['operation'] == 'git/push':
                if kind != 'exact' or predicates: raise ValueError('Git push permissions must be exact and single use')
                if self.git_reviews.get(row['fingerprint'],(0,))[0] <= self.now(): raise ValueError('Git review expired; retry the push to regenerate it')
            if row['operation'] == 'git/read' and predicates: raise ValueError('Git read sessions do not support body predicates')
            if row['operation']=='pulls/create' and self.rest_reviews.get(row['fingerprint'],(0,))[0]<=self.now():
                raise ValueError('pull request review expired; retry the request')
            grant_id = str(uuid.uuid4()); expires = self.now()+ttl
            self.audit.emit('approval.granted', request_id=request_id, grant_id=grant_id, actor={'type':'human', 'interface':'host_browser'}, grant={'kind':kind, 'expires_at':expires, 'operation':row['operation'], 'path':row['path'], 'body_equals':predicates, 'max_uses':1 if kind=='exact' else None})
            with self.db:
                self.db.execute('INSERT INTO grants(id,request_id,kind,fingerprint,operation,path,predicates,expires,remaining) VALUES(?,?,?,?,?,?,?,?,?)', (grant_id, request_id, kind, row['fingerprint'], row['operation'], row['path'], dumps(predicates), expires, 1 if kind=='exact' else -1))
                self.db.execute("UPDATE requests SET status='approved' WHERE id=?", (request_id,))
            return {'grant_id':grant_id, 'expires_at':expires}
    def git_review(self, request_id):
        with self.lock:
            row = self.db.execute('SELECT fingerprint FROM requests WHERE id=? AND operation=?', (request_id,'git/push')).fetchone()
            cached = self.git_reviews.get(row['fingerprint']) if row else None
            if not cached or cached[0] <= self.now(): raise ValueError('review expired; retry the push')
            return {'expires_at':cached[0], **cached[1]}
    def request_review(self, request_id):
        with self.lock:
            row=self.db.execute('SELECT fingerprint,operation FROM requests WHERE id=?',(request_id,)).fetchone()
            if row and row['operation']=='git/push': return self.git_review(request_id)
            cached=self.rest_reviews.get(row['fingerprint']) if row and row['operation']=='pulls/create' else None
            if not cached or cached[0]<=self.now(): raise ValueError('review expired; retry the request')
            return {'expires_at':cached[0],'pull_request':cached[1]}
    def prune_reviews(self):
        with self.lock:
            now=self.now()
            self.git_reviews={k:v for k,v in self.git_reviews.items() if v[0]>now}
            self.rest_reviews={k:v for k,v in self.rest_reviews.items() if v[0]>now}
    def deny(self, request_id):
        with self.lock:
            self.audit.emit('approval.denied', request_id=request_id, actor={'type':'human'})
            with self.db: self.db.execute("UPDATE requests SET status='denied' WHERE id=?", (request_id,))
    def revoke(self, grant_id):
        with self.lock:
            self.audit.emit('approval.revoked', grant_id=grant_id, actor={'type':'human'})
            with self.db: self.db.execute('UPDATE grants SET revoked=1 WHERE id=?', (grant_id,))
    def active(self, decision_id):
        with self.lock:
            if not self.network_enabled:return False
            if decision_id in self.network_decisions:return self.network_decisions[decision_id]>self.now()
            row = self.db.execute('SELECT d.expires,g.revoked FROM decisions d JOIN grants g ON d.grant_id=g.id WHERE d.id=?', (decision_id,)).fetchone()
            return bool(row and not row['revoked'] and row['expires'] > self.now())
    def snapshot(self):
        with self.lock:
            from .pages import history_page
            requests = history_page(self,{'status':'pending'})['items']
            pending_count = self.db.execute("SELECT count(*) FROM requests WHERE status='pending'").fetchone()[0]
            grants = [dict(r) for r in self.db.execute('SELECT id,request_id,kind,operation,path,predicates,expires,remaining,revoked FROM grants ORDER BY expires DESC LIMIT 100')]
            for g in grants: g['active'] = not g['revoked'] and g['expires']>self.now() and g['remaining']!=0
            return self.redactor.clean({'requests':requests, 'pending_count':pending_count, 'grants':grants, 'policy':self.policy, 'network_enabled':self.network_enabled, 'token_configured':bool(self.token), 'github_app':self.github_app.snapshot() if self.github_app else None, 'figma':self.figma.snapshot(), 'google_docs':self.google_docs.snapshot(), 'time':self.now(), 'catalog_revision':self.operations.revision, 'catalog_operations':len(self.operations.routes), 'audit_path':str(self.audit.path)})
