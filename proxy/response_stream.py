"""Bounded byte inspection for provider SSE; transport chunks are not events."""
from __future__ import annotations

import zlib

PROVIDER_ENDPOINTS = frozenset({
    ('api.openai.com', '/v1/responses'),
    ('api.openai.com', '/v1/chat/completions'),
    ('chatgpt.com', '/backend-api/codex/responses'),
})
RESPONSE_LIMIT = 16 * 1024 * 1024


def provider_request(request):
    return (request.scheme == 'https' and request.port == 443
            and request.method == 'POST'
            and (request.host, request.path) in PROVIDER_ENDPOINTS)


class StreamRejected(ValueError):
    """Only fixed, non-sensitive reasons may cross into audit records."""


class ResponseStream:
    def __init__(self, secrets, encoding='identity', limit=RESPONSE_LIMIT):
        if encoding not in ('identity', 'gzip', 'deflate'):
            raise StreamRejected('unsupported response stream encoding')
        self.secrets = tuple(secret.encode() for secret in secrets if secret)
        self.pending = b''
        self.limit = limit
        self.received = self.decoded = self.delivered = 0
        self.finished = False
        self.reason = None
        self.decoder = (zlib.decompressobj(31 if encoding == 'gzip' else 15)
                        if encoding != 'identity' else None)

    def feed(self, chunk):
        if self.reason or self.finished:
            return b''
        self.received += len(chunk)
        if self.received > self.limit:
            raise StreamRejected('response exceeds inspection limit')
        data = chunk
        if self.decoder:
            try:
                # Bound expansion as well as wire bytes, including compression bombs.
                data = self.decoder.decompress(chunk, self.limit - self.decoded + 1)
            except zlib.error:
                raise StreamRejected('invalid response stream encoding') from None
            if self.decoder.unused_data:
                raise StreamRejected('trailing compressed response data')
            if not chunk and not self.decoder.eof:
                raise StreamRejected('truncated compressed response')
        self.decoded += len(data)
        if self.decoded > self.limit:
            raise StreamRejected('response exceeds inspection limit')
        data = self.pending + data
        if any(secret in data for secret in self.secrets):
            raise StreamRejected('provider response exposed a protected credential')
        # Retain only suffixes that could become a secret with the next chunk.
        # Ordinary SSE events therefore pass immediately, even with long tokens.
        keep = 0
        if chunk:
            for secret in self.secrets:
                for size in range(min(len(secret) - 1, len(data)), keep, -1):
                    if data.endswith(secret[:size]):
                        keep = size
                        break
        output = data[:-keep] if keep else data
        self.pending = data[-keep:] if keep else b''
        self.delivered += len(output)
        self.finished = not chunk
        return output

    def clear(self):
        self.pending = b''
        self.secrets = ()
        self.decoder = None

    def summary(self, phase):
        return {'phase': phase, 'received_bytes': self.received,
                'decoded_bytes': self.decoded, 'forwarded_bytes': self.delivered}
