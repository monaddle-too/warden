"""Fixed local document API dispatch. Signing keys never enter gateway or guest.

Only the broker constructs run attribution. The HTTP transport ignores proxy
settings, does not follow redirects, and never forwards caller-supplied headers.
"""
from __future__ import annotations

import base64
import hashlib
import hmac
import http.client
import json
import os
import re
import secrets
import socket
import stat
import threading
import time
from urllib.parse import urlsplit

MAX_BODY = 2 * 1024 * 1024
MAX_RESPONSE = 4 * 1024 * 1024
PREFIX = '/workspace/v1/documents'
UPSTREAM_PREFIX = '/agent/v1/documents'
DOCUMENT_ID = r'[A-Za-z0-9][A-Za-z0-9_-]{0,127}'


def document_path(method, path, project_id):
    if not isinstance(path, str) or len(path) > 512:
        raise ValueError('invalid document route')
    # No encoded separators, aliases, fragments or target overrides. The only
    # queries are an optional bound project ID and bounded pagination offset.
    query = ''
    if '?' in path:
        path, query = path.split('?', 1)
        fields = query.split('&')
        names = set()
        for field in fields:
            key, sep, value = field.partition('=')
            if not sep or key in names:
                raise ValueError('invalid document query')
            names.add(key)
            if key == 'projectID' and path == PREFIX and value == project_id:
                continue
            if (key == 'offset' and method == 'GET' and re.fullmatch(r'[0-9]{1,9}', value)
                    and (path == PREFIX or path.endswith(('/revisions', '/proposals', '/comments/replies')))):
                continue
            raise ValueError('invalid document query')
        query = '?' + query
    suffix = path.removeprefix(PREFIX)
    valid = (method == 'GET' and (path == PREFIX or re.fullmatch(
        PREFIX + '/' + DOCUMENT_ID + r'(?:/(?:revisions|proposals|comments/replies))?', path)))
    valid = valid or (method == 'POST' and re.fullmatch(
        PREFIX + '/' + DOCUMENT_ID + r'(?:/proposals|/comments/' + DOCUMENT_ID + '/replies)', path))
    if not valid:
        raise ValueError('unsupported document route')
    return UPSTREAM_PREFIX + suffix + query


class DocumentAPI:
    def __init__(self, origin, key_file, clock=time.time):
        parsed = urlsplit(origin)
        if (parsed.scheme != 'http' or parsed.hostname not in ('localhost', '127.0.0.1', '::1')
                or parsed.username is not None or parsed.password is not None
                or parsed.path or parsed.query or parsed.fragment
                or origin != 'http://' + parsed.netloc or not parsed.port
                or not 1 <= parsed.port <= 65535):
            raise ValueError('document API origin must be a canonical HTTP loopback origin with port')
        self.host = '127.0.0.1' if parsed.hostname == 'localhost' else parsed.hostname
        self.authority = parsed.netloc
        self.port = parsed.port
        self.clock = clock
        fd = os.open(key_file, os.O_RDONLY | os.O_NOFOLLOW)
        try:
            info = os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                    or info.st_mode & 0o077 or info.st_size > 4096):
                raise ValueError('document API key must be a private owned regular file')
            self.key = os.read(fd, 4097).rstrip()
        finally:
            os.close(fd)
        if not 32 <= len(self.key) <= 4096 or any(c < 33 or c > 126 for c in self.key):
            raise ValueError('document API key must contain at least 32 printable ASCII characters')

    def prepare(self, method, path, body, context):
        path = document_path(method, path, context['projectID'])
        if not isinstance(body, bytes) or len(body) > MAX_BODY:
            raise ValueError('document body too large')
        if method == 'GET' and body:
            raise ValueError('document reads must not have a body')
        if method == 'POST':
            parsed = json.loads(body)
            if not isinstance(parsed, dict):
                raise ValueError('document mutation must be a JSON object')
        encoded = base64.urlsafe_b64encode(json.dumps(context, separators=(',', ':'), sort_keys=True).encode()).rstrip(b'=').decode()
        timestamp = str(int(self.clock()))
        nonce = secrets.token_hex(32)
        canonical = '\n'.join((method, path, hashlib.sha256(body).hexdigest(), timestamp, nonce, encoded))
        signature = hmac.new(self.key, canonical.encode(), hashlib.sha256).hexdigest()
        headers = {'Host': self.authority, 'Content-Type': 'application/json', 'Accept': 'application/json',
            'Accept-Encoding': 'identity', 'X-Warden-Context': encoded, 'X-Warden-Timestamp': timestamp,
            'X-Warden-Nonce': nonce, 'X-Warden-Signature': signature}
        return path, headers

    def dispatch(self, method, path, body, headers):
        # http.client neither honors HTTP_PROXY nor follows Location redirects.
        with_connection = http.client.HTTPConnection(self.host, self.port, timeout=5)
        deadline = None
        try:
            with_connection.connect()
            transport = with_connection.sock
            def interrupt():
                try:
                    transport.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
            # Socket read timeouts alone permit an endless trickle response.
            # Bound the whole HTTP exchange, including headers and chunk frames.
            deadline = threading.Timer(5, interrupt)
            deadline.daemon = True
            deadline.start()
            with_connection.request(method, path, body=body, headers=headers)
            response = with_connection.getresponse()
            if (response.status not in range(200, 300) and response.status not in (400, 401, 403, 404, 409, 413, 422, 429)):
                raise ValueError('document upstream returned unsupported status')
            if (response.getheader('Content-Type', '').split(';', 1)[0].strip().lower() != 'application/json'
                    or response.getheader('Content-Encoding', 'identity').lower() != 'identity'):
                raise ValueError('document upstream must return uncompressed JSON')
            data = response.read(MAX_RESPONSE + 1)
            if len(data) > MAX_RESPONSE:
                raise ValueError('document response too large')
            parsed = json.loads(data)
            if not isinstance(parsed, (dict, list)):
                raise ValueError('document response must be a JSON object or array')
            # Never send the key, request signature or trusted context back to a
            # guest even if a misconfigured local upstream reflects its input.
            inspected = json.dumps(parsed, ensure_ascii=False).encode() + b'\n' + data
            protected = (self.key, headers['X-Warden-Signature'].encode(), headers['X-Warden-Context'].encode())
            if any(value in inspected for value in protected):
                raise ValueError('document response exposed protected authentication')
            return {'status': response.status, 'body': base64.b64encode(data).decode()}
        finally:
            if deadline:
                deadline.cancel()
            with_connection.close()
