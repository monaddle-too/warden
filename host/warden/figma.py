"""Figma REST adapter and host-only OAuth connection.

Credentials and PKCE state live only in the control-plane process. No generic
URL fetcher, client-supplied upstream, OAuth token passthrough, or MCP forwarding.
"""
from __future__ import annotations

import base64
import hashlib
import http.client
import json
import re
import secrets
import ssl
import time
from urllib.parse import urlencode, urlsplit

SCOPES = ('current_user:read', 'file_content:read', 'file_metadata:read', 'file_comments:read')
FILE_KEY = re.compile(r'[A-Za-z0-9_-]{1,128}')


def protected_host(host):
    host = host.lower().rstrip('.')
    return any(host == suffix or host.endswith('.' + suffix)
               for suffix in ('figma.com', 'figma-gov.com', 'figmausercontent.com'))


def operation(method, path, query, body):
    if method != 'GET' or body:
        raise ValueError('only supported Figma reads are available')
    if path == '/v1/me':
        name, key, allowed = 'figma/users/me', '', set()
    else:
        match = re.fullmatch(r'/v1/files/([A-Za-z0-9_-]{1,128})(?:/(nodes|meta|comments))?', path)
        if not match:
            raise ValueError('unsupported Figma operation')
        key, suffix = match.groups()
        name = 'figma/files/' + (suffix or 'read')
        allowed = {'version', 'ids', 'depth'} if suffix in (None, 'nodes') else ({'as_md'} if suffix == 'comments' else set())
        if suffix == 'nodes' and not dict(query).get('ids'):
            raise ValueError('Figma node reads require ids')
    for field, value in query:
        if field not in allowed:
            raise ValueError('unsupported Figma query parameter')
        if field == 'depth' and not re.fullmatch(r'[1-9][0-9]{0,2}', value):
            raise ValueError('invalid Figma depth')
        if field == 'ids' and not re.fullmatch(r'[0-9]+:[0-9]+(?:,[0-9]+:[0-9]+)*', value):
            raise ValueError('invalid Figma node ids')
        if field == 'version' and not re.fullmatch(r'[A-Za-z0-9_-]{1,128}', value):
            raise ValueError('invalid Figma version')
        if field == 'as_md' and value not in ('true', 'false'):
            raise ValueError('invalid Figma comment format')
    return {'operation_id': name}, key


def exchange(client_id, client_secret, endpoint, fields):
    """Fixed HTTPS authority and paths, bounded JSON, and no redirects/proxies."""
    if endpoint not in ('/v1/oauth/token', '/v1/oauth/refresh'):
        raise ValueError('unsupported OAuth endpoint')
    connection = http.client.HTTPSConnection('api.figma.com', timeout=10, context=ssl.create_default_context())
    auth = base64.b64encode((client_id + ':' + client_secret).encode()).decode()
    try:
        connection.request('POST', endpoint, body=urlencode(fields), headers={
            'Authorization': 'Basic ' + auth, 'Content-Type': 'application/x-www-form-urlencoded',
            'Accept': 'application/json'})
        response = connection.getresponse()
        data = response.read(65537)
        if response.status != 200 or len(data) > 65536:
            raise ValueError('Figma authorization failed; reconnect your account')
        return json.loads(data)
    except (OSError, http.client.HTTPException, json.JSONDecodeError):
        raise ValueError('Figma authorization unavailable; retry connecting') from None
    finally:
        connection.close()


class Connection:
    """Called under Engine.lock; serializes token refresh and OAuth completion."""
    def __init__(self, redactor, clock=time.time, transport=exchange):
        self.redactor, self.clock, self.transport = redactor, clock, transport
        self.client_id = self.client_secret = self.redirect_uri = None
        self.pending = None
        self.access_token = self.refresh_token = self.user_id = None
        self.expires = 0

    def configure(self, client_id, client_secret, redirect_uri):
        if not isinstance(client_id, str) or not re.fullmatch(r'[A-Za-z0-9_-]{1,256}', client_id):
            raise ValueError('invalid Figma client ID')
        if not isinstance(client_secret, str) or not re.fullmatch(r'[!-~]{8,4096}', client_secret):
            raise ValueError('invalid Figma client secret')
        uri = urlsplit(redirect_uri)
        if (uri.scheme != 'http' or uri.hostname != '127.0.0.1' or not uri.port
                or uri.netloc != '127.0.0.1:' + str(uri.port)
                or uri.path != '/oauth/figma/callback' or uri.query or uri.fragment):
            raise ValueError('Figma callback must be the local control-plane URL')
        self.disconnect()
        self.client_id, self.client_secret, self.redirect_uri = client_id, client_secret, redirect_uri
        self.redactor.register(client_secret)

    def start(self):
        if not self.client_id:
            raise ValueError('configure a Figma OAuth app first')
        state, verifier = secrets.token_urlsafe(32), secrets.token_urlsafe(64)
        self.redactor.register(state)
        self.redactor.register(verifier)
        self.pending = (state, verifier, self.clock() + 600)
        challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip('=')
        return 'https://www.figma.com/oauth?' + urlencode({
            'client_id': self.client_id, 'redirect_uri': self.redirect_uri, 'scope': ' '.join(SCOPES),
            'state': state, 'response_type': 'code', 'code_challenge': challenge, 'code_challenge_method': 'S256'})

    def complete(self, state, code):
        pending = self.pending
        if (not pending or not isinstance(state, str) or not secrets.compare_digest(state, pending[0])
                or self.clock() >= pending[2]):
            raise ValueError('invalid or expired Figma connection request')
        self.pending = None  # Single use, including failed token exchanges.
        if not isinstance(code, str) or not re.fullmatch(r'[!-~]{1,4096}', code):
            raise ValueError('invalid Figma authorization code')
        self.redactor.register(code)
        result = self.transport(self.client_id, self.client_secret, '/v1/oauth/token', {
            'code': code, 'redirect_uri': self.redirect_uri, 'grant_type': 'authorization_code',
            'code_verifier': pending[1]})
        user_id = result.get('user_id_string', result.get('user_id')) if isinstance(result, dict) else None
        if type(user_id) not in (str, int) or not re.fullmatch(r'[0-9]{1,128}', str(user_id)):
            raise ValueError('Figma did not return an account identity')
        self._install(result, initial=True)
        self.user_id = str(user_id)

    def _install(self, data, initial=False):
        if not isinstance(data, dict):
            raise ValueError('invalid Figma token response')
        token = data.get('access_token')
        refresh = data.get('refresh_token', None if initial else self.refresh_token)
        ttl = data.get('expires_in')
        if (any(not isinstance(v, str) or not re.fullmatch(r'[A-Za-z0-9._~+/=-]{16,8192}', v)
                for v in (token, refresh)) or type(ttl) not in (int, float) or not 60 < ttl <= 366 * 86400
                or data.get('token_type', 'bearer').lower() != 'bearer'):
            raise ValueError('invalid Figma token response')
        if 'scope' in data and (not isinstance(data['scope'], str)
                or set(data['scope'].replace(',', ' ').split()) - set(SCOPES)):
            raise ValueError('unexpected Figma OAuth scopes')
        self.redactor.register(token)
        self.redactor.register(refresh)
        self.access_token, self.refresh_token, self.expires = token, refresh, self.clock() + ttl

    def authorization(self):
        if not self.access_token:
            raise ValueError('connect your Figma account in Warden')
        if self.clock() >= self.expires - 60:
            result = self.transport(self.client_id, self.client_secret, '/v1/oauth/refresh', {'refresh_token': self.refresh_token})
            self._install(result)
        return 'Bearer ' + self.access_token

    def disconnect(self):
        self.pending = None
        self.access_token = self.refresh_token = self.user_id = None
        self.expires = 0

    def snapshot(self):
        return {'app_configured': bool(self.client_id), 'connected': bool(self.access_token),
                'account_id': self.user_id, 'expires_at': self.expires if self.access_token else None,
                'scopes': list(SCOPES), 'storage': 'host_memory'}
