"""Read an existing host-owned Codex login without exporting it to the guest.

The pinned Codex 0.154.0 model-provider-info and model-provider/auth modules
specify this exact upstream and account header. No refresh token is returned or
used: an expired login requires the owning Codex application to refresh it.
"""
from __future__ import annotations

import base64
import json
import os
from pathlib import Path
import re
import stat
import time


class CodexCredentials:
    def __init__(self, path, clock=time.time):
        self.path = Path(path)
        self.clock = clock

    def _read(self):
        # Open the same regular, private, host-owned inode we check. Never follow
        # a swapped final symlink or copy a refresh token into another store.
        fd = os.open(self.path, os.O_RDONLY | os.O_NOFOLLOW)
        try:
            info = os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                    or info.st_mode & 0o077 or info.st_size > 1024 * 1024):
                raise ValueError('Codex credential file must be private and owned')
            with os.fdopen(fd, 'rb', closefd=False) as stream:
                data = json.loads(stream.read(1024 * 1024 + 1))
        finally:
            os.close(fd)
        if not isinstance(data, dict):
            raise ValueError('Invalid host credential document')
        tokens = data.get('tokens')
        if data.get('auth_mode') != 'chatgpt' or not isinstance(tokens, dict):
            raise ValueError('A host ChatGPT login is required')
        token, account = tokens.get('access_token'), tokens.get('account_id')
        if (not isinstance(token, str) or len(token) > 65536
                or not re.fullmatch(r'[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+', token)
                or not isinstance(account, str)
                or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}', account)):
            raise ValueError('Invalid host ChatGPT credential shape')
        payload = token.split('.')[1]
        claims = json.loads(base64.urlsafe_b64decode(payload + '=' * (-len(payload) % 4)))
        if not isinstance(claims, dict):
            raise ValueError('Invalid host credential claims')
        expiry = claims.get('exp')
        # JWT signature verification belongs to the fixed TLS upstream. This
        # local expiry check only refuses stale credentials before disclosure.
        if type(expiry) not in (int, float) or expiry <= self.clock() + 30:
            raise ValueError('Refresh the host Codex sign-in before running a chat')
        return token, account

    def available(self):
        try:
            self._read()
            return True
        except (OSError, ValueError, TypeError, KeyError):
            return False

    def route(self, api_path):
        if api_path != '/v1/responses':
            raise ValueError('This host login supports only Codex Responses')
        return {'host': 'chatgpt.com', 'path': '/backend-api/codex/responses'}

    def headers(self):
        token, account = self._read()
        return {'Authorization': 'Bearer ' + token, 'ChatGPT-Account-ID': account}


class ClaudeCredentials(CodexCredentials):
    """Host-owned Claude Code login. Only the access token reaches the gateway."""
    def _read(self):
        fd = os.open(self.path, os.O_RDONLY | os.O_NOFOLLOW)
        try:
            info = os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                    or info.st_mode & 0o077 or info.st_size > 1024 * 1024):
                raise ValueError('Claude credential file must be private and owned')
            with os.fdopen(fd, 'rb', closefd=False) as stream:
                data = json.load(stream)
        finally:
            os.close(fd)
        if not isinstance(data, dict): raise ValueError('invalid Claude credential document')
        oauth = data.get('claudeAiOauth', {})
        if not isinstance(oauth, dict): raise ValueError('invalid Claude credential document')
        token, expiry = oauth.get('accessToken'), oauth.get('expiresAt')
        if (not isinstance(token, str) or not re.fullmatch(r'[A-Za-z0-9_.-]{16,4096}', token)
                or 'proxy' in token.lower() or 'placeholder' in token.lower()
                or type(expiry) not in (int, float) or expiry / 1000 <= self.clock() + 30):
            raise ValueError('Refresh the Claude sign-in before running a chat')
        return token

    def route(self, api_path):
        if api_path not in ('/v1/messages', '/v1/messages/count_tokens'):
            raise ValueError('unsupported Claude route')
        return {'host': 'api.anthropic.com', 'path': api_path}

    def headers(self):
        return {'Authorization': 'Bearer ' + self._read()}
