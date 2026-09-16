"""Google Docs REST adapter and host-only OAuth connection.

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

SCOPES = ('https://www.googleapis.com/auth/documents.readonly',)
DOCUMENT_ID = re.compile(r'[A-Za-z0-9_-]{1,256}')


def protected_host(host):
    host = host.lower().rstrip('.')
    # Protect alternate Docs/Drive API and web routes, including batch endpoints.
    return (host == 'docs.google.com' or host == 'drive.google.com'
            or host == 'googleapis.com' or host.endswith('.googleapis.com'))


def operation(method, path, query, body):
    if method != 'GET' or body:
        raise ValueError('only supported Google Docs reads are available')
    match = re.fullmatch(r'/v1/documents/([A-Za-z0-9_-]{1,256})', path)
    if not match:
        raise ValueError('unsupported Google Docs operation')
    for field, value in query:
        if field == 'includeTabsContent' and value in ('true', 'false'):
            continue
        if field == 'suggestionsViewMode' and value in (
                'DEFAULT_FOR_CURRENT_ACCESS', 'SUGGESTIONS_INLINE',
                'PREVIEW_SUGGESTIONS_ACCEPTED', 'PREVIEW_WITHOUT_SUGGESTIONS'):
            continue
        raise ValueError('unsupported Google Docs query parameter')
    return {'operation_id':'google_docs/documents/get'}, match[1]


def exchange(client_id, client_secret, endpoint, fields):
    """Fixed HTTPS authority and paths, bounded JSON, and no redirects/proxies."""
    if endpoint not in ('/token',):
        raise ValueError('unsupported OAuth endpoint')
    connection = http.client.HTTPSConnection('oauth2.googleapis.com', timeout=10, context=ssl.create_default_context())
    fields = {**fields, 'client_id':client_id, 'client_secret':client_secret}
    try:
        connection.request('POST', endpoint, body=urlencode(fields), headers={
            'Content-Type': 'application/x-www-form-urlencoded',
            'Accept': 'application/json'})
        response = connection.getresponse()
        data = response.read(65537)
        if response.status != 200 or len(data) > 65536:
            raise ValueError('Google Docs authorization failed; reconnect your account')
        return json.loads(data)
    except (OSError, http.client.HTTPException, json.JSONDecodeError):
        raise ValueError('Google Docs authorization unavailable; retry connecting') from None
    finally:
        connection.close()


class Connection:
    """Called under Engine.lock; serializes token refresh and OAuth completion."""
    def __init__(self, redactor, clock=time.time, transport=exchange):
        self.redactor, self.clock, self.transport = redactor, clock, transport
        self.client_id = self.client_secret = self.redirect_uri = None
        self.pending = None
        self.access_token = self.refresh_token = None
        self.expires = 0

    def configure(self, client_id, client_secret, redirect_uri, *, allow_https=False):
        if not isinstance(client_id, str) or not re.fullmatch(r'[A-Za-z0-9_-]{1,256}\.apps\.googleusercontent\.com', client_id):
            raise ValueError('invalid Google Docs client ID')
        if not isinstance(client_secret, str) or not re.fullmatch(r'[!-~]{8,4096}', client_secret):
            raise ValueError('invalid Google Docs client secret')
        uri = urlsplit(redirect_uri)
        local = (uri.scheme == 'http' and uri.hostname == '127.0.0.1' and uri.port
                 and uri.netloc == '127.0.0.1:' + str(uri.port))
        public = (allow_https and uri.scheme == 'https' and uri.hostname
                  and re.fullmatch(r'[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+', uri.hostname)
                  and uri.netloc == uri.hostname)
        if (not (local or public) or uri.path != '/oauth/google_docs/callback' or uri.query or uri.fragment):
            raise ValueError('Google Docs callback must be the local control-plane URL')
        self.disconnect()
        self.client_id, self.client_secret, self.redirect_uri = client_id, client_secret, redirect_uri
        self.redactor.register(client_secret)

    def start(self):
        if not self.client_id:
            raise ValueError('configure a Google Docs OAuth app first')
        state, verifier = secrets.token_urlsafe(32), secrets.token_urlsafe(64)
        self.redactor.register(state)
        self.redactor.register(verifier)
        self.pending = (state, verifier, self.clock() + 600)
        challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip('=')
        return 'https://accounts.google.com/o/oauth2/v2/auth?' + urlencode({
            'client_id': self.client_id, 'redirect_uri': self.redirect_uri, 'scope': ' '.join(SCOPES),
            'state': state, 'response_type': 'code', 'access_type':'offline', 'prompt':'consent', 'code_challenge': challenge, 'code_challenge_method': 'S256'})

    def complete(self, state, code):
        pending = self.pending
        if (not pending or not isinstance(state, str) or not secrets.compare_digest(state, pending[0])
                or self.clock() >= pending[2]):
            raise ValueError('invalid or expired Google Docs connection request')
        self.pending = None  # Single use, including failed token exchanges.
        if not isinstance(code, str) or not re.fullmatch(r'[!-~]{1,4096}', code):
            raise ValueError('invalid Google Docs authorization code')
        self.redactor.register(code)
        result = self.transport(self.client_id, self.client_secret, '/token', {
            'code': code, 'redirect_uri': self.redirect_uri, 'grant_type': 'authorization_code',
            'code_verifier': pending[1]})
        self._install(result, initial=True)

    def _install(self, data, initial=False):
        if not isinstance(data, dict):
            raise ValueError('invalid Google Docs token response')
        token = data.get('access_token')
        refresh = data.get('refresh_token', None if initial else self.refresh_token)
        ttl = data.get('expires_in')
        if (any(not isinstance(v, str) or not re.fullmatch(r'[A-Za-z0-9._~+/=-]{16,8192}', v)
                for v in (token, refresh)) or type(ttl) not in (int, float) or not 60 < ttl <= 366 * 86400
                or data.get('token_type', 'bearer').lower() != 'bearer'):
            raise ValueError('invalid Google Docs token response')
        if (not isinstance(data.get('scope'), str)
                or set(data['scope'].split()) != set(SCOPES)):
            raise ValueError('unexpected Google Docs OAuth scopes')
        self.redactor.register(token)
        self.redactor.register(refresh)
        self.access_token, self.refresh_token, self.expires = token, refresh, self.clock() + ttl

    def authorization(self):
        if not self.access_token:
            raise ValueError('connect your Google Docs account in Warden')
        if self.clock() >= self.expires - 60:
            result = self.transport(self.client_id, self.client_secret, '/token', {'refresh_token': self.refresh_token, 'grant_type':'refresh_token'})
            self._install(result)
        return 'Bearer ' + self.access_token

    def disconnect(self):
        self.pending = None
        self.access_token = self.refresh_token = None
        self.expires = 0

    def snapshot(self):
        return {'app_configured': bool(self.client_id), 'connected': bool(self.access_token),
                'expires_at': self.expires if self.access_token else None,
                'scopes': list(SCOPES), 'storage': 'host_memory'}
