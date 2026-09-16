"""Fixed-identity SBX gateway; launched only by trusted host code.

Regular explicit proxy plus a narrow OpenAI reverse route. There is deliberately
no guest-selectable registry identity, raw TCP forwarding or Apple TLS bypass.
"""
from __future__ import annotations

import asyncio
import base64
import contextvars
import json
import hashlib
import hmac
import os
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'host'))
sys.path.insert(0, str(ROOT / 'proxy'))
from addon import Guard
from warden.sbx import rpc, PROVIDER_ROUTES
from warden.egress import HOST
from warden.sbx_documents import PREFIX as DOCUMENT_PREFIX, MAX_BODY as DOCUMENT_MAX_BODY

CURRENT_FLOW = contextvars.ContextVar('sbx_flow', default=None)


class SbxGuard(Guard):
    def __init__(self, socket_path, binding_id, capability, gateway_port):
        super().__init__()
        self.socket_path = socket_path
        self.binding_id = binding_id
        self.capability = capability
        self.gateway_port = gateway_port

    async def control(self, message):
        flow = CURRENT_FLOW.get()
        if message.get('action') == 'egress' and flow is not None:
            message = {**message, 'request': {**message['request'], 'path': flow.request.path}}
        result = await asyncio.to_thread(rpc, self.socket_path, {
            'version': 1, 'operation': 'proxy', 'bindingID': self.binding_id,
            'capability': self.capability, 'message': message})
        return result

    async def running(self):
        # No Linux heartbeat, firewall commands, readiness assertions or vsock.
        # Readiness is established by independent verifier code in the broker.
        parent = os.environ.get('WARDEN_SBX_PARENT_PID')
        if parent:
            async def parent_watch():
                while True:
                    if os.getppid() != int(parent):
                        from mitmproxy import ctx
                        ctx.master.shutdown()
                        return
                    await asyncio.sleep(3)
            self.heartbeat = asyncio.create_task(parent_watch())

    async def tls_clienthello(self, data):
        # Never inherit the macOS updater's end-to-end TLS exception.
        data.ignore_connection = False
        data.establish_server_tls_first = False

    async def http_connect(self, flow):
        host = flow.request.host
        if (not isinstance(host, str) or not HOST.fullmatch(host)
                or flow.request.port != 443 or flow.request.headers.get('Origin')):
            await self.deny(flow, 'only inspected HTTPS CONNECT is supported')
            return
        try:
            decision = await self.control({'action': 'destination', 'request': {
                'host': host, 'method': 'GET', 'scheme': 'https'}})
            if not decision.get('allow'):
                await self.deny(flow, 'destination or active lease denied', 403)
        except Exception:
            await self.deny(flow, 'gateway control unavailable', 503)
        # The regular-mode CONNECT hook does not dispatch a TCP connection with
        # connection_strategy=lazy. Subsequent HTTP still passes Guard.request.

    async def requestheaders(self, flow):
        if flow.request.headers.get('Origin') is not None:
            await self.deny(flow, 'browser origins are not allowed at the gateway')
            return
        if flow.request.path.startswith(DOCUMENT_PREFIX):
            try:
                if int(flow.request.headers.get('Content-Length', '0')) > DOCUMENT_MAX_BODY:
                    await self.deny(flow, 'document request exceeds limit', 413)
                    return
            except ValueError:
                await self.deny(flow, 'invalid document content length')
                return
        await super().requestheaders(flow)

    async def document_request(self, flow):
        from mitmproxy import http
        req = flow.request
        flow.metadata['sbx_document'] = True
        try:
            if (req.scheme != 'http' or req.port != self.gateway_port
                    or req.headers.get('Content-Encoding', 'identity').lower() != 'identity'
                    or len(req.raw_content or b'') > DOCUMENT_MAX_BODY
                    or (req.method == 'POST' and req.headers.get('Content-Type', '').split(';', 1)[0].strip().lower() != 'application/json')):
                raise ValueError('invalid document request')
            body = req.raw_content or b''
            # A fresh header set also removes any forged Warden identity,
            # authentication, forwarding headers and browser cookies from flows.
            req.headers.clear()
            result = await self.control({'action': 'document', 'method': req.method,
                'path': req.path, 'body': base64.b64encode(body).decode()})
            if not result.get('allow'):
                flow.response = http.Response.make(result.get('status', 503), b'{"error":"document access unavailable"}',
                    {'Content-Type': 'application/json', 'Cache-Control': 'no-store'})
                return
            flow.response = http.Response.make(result['status'], base64.b64decode(result['body'], validate=True),
                {'Content-Type': 'application/json', 'Cache-Control': 'no-store'})
        except Exception:
            req.headers.clear()
            flow.response = http.Response.make(503, b'{"error":"document gateway unavailable"}',
                {'Content-Type': 'application/json', 'Cache-Control': 'no-store'})

    async def responseheaders(self, flow):
        if flow.metadata.get('sbx_document'):
            flow.response.stream = False
            return
        await super().responseheaders(flow)

    async def request(self, flow):
        req = flow.request
        if flow.metadata.get('warden_denied'):
            return
        if (req.method == 'GET' and req.host == '127.0.0.1' and req.port == self.gateway_port
                and req.path.startswith('/__warden_sbx_health/')):
            nonce = req.path.removeprefix('/__warden_sbx_health/')
            if len(nonce) == 64 and all(c in '0123456789abcdef' for c in nonce):
                from mitmproxy import http
                proof = hmac.new(self.capability.encode(), nonce.encode(), hashlib.sha256).hexdigest()
                flow.metadata['sbx_health'] = True
                flow.response = http.Response.make(200, json.dumps({'bindingID': self.binding_id, 'mac': proof}).encode(), {'Content-Type': 'application/json', 'Cache-Control': 'no-store'})
                return
        reverse = req.host in ('host.docker.internal', 'localhost', '127.0.0.1')
        if reverse and req.path.startswith(DOCUMENT_PREFIX):
            await self.document_request(flow)
            return
        if reverse:
            prefix = '/anthropic' if req.path.startswith('/anthropic/') else '/openai'
            api_path = req.path[len(prefix):]
            if prefix == '/anthropic' and api_path.endswith('?beta=true'): api_path = api_path.removesuffix('?beta=true')
            # A host alias only selects this exact provider; it is not a target
            # override or arbitrary reverse proxy. No cookies/guest auth survive.
            if (req.scheme != 'http' or req.port != self.gateway_port
                    or not req.path.startswith(prefix + '/')
                    or api_path not in PROVIDER_ROUTES
                    or req.method != 'POST'):
                await self.deny(flow, 'unsupported provider route')
                return
            try:
                route = await self.control({'action': 'providerRoute', 'path': api_path})
                if not route.get('allow') or (route.get('host'), route.get('path')) not in (
                        ('api.openai.com', '/v1/responses'), ('api.openai.com', '/v1/chat/completions'),
                        ('chatgpt.com', '/backend-api/codex/responses'),
                        ('api.anthropic.com', '/v1/messages'), ('api.anthropic.com', '/v1/messages/count_tokens')):
                    await self.deny(flow, 'provider route unavailable', 503)
                    return
            except Exception:
                await self.deny(flow, 'gateway control unavailable', 503)
                return
            req.path = route['path']
            req.scheme = 'https'
            req.host = route['host']
            req.port = 443
            req.headers['Host'] = route['host']
            # The trusted reverse mapping fixes the upstream TLS authority. This
            # value is not guest TLS evidence; ordinary explicit flows retain
            # their actual SNI and are checked by Guard.
            flow.client_conn.sni = route['host']
        if req.host in ('api.openai.com', 'chatgpt.com', 'api.anthropic.com', 'docs.googleapis.com'):
            for header in ('Authorization', 'Proxy-Authorization', 'Cookie', 'OpenAI-Organization', 'OpenAI-Project', 'ChatGPT-Account-ID', 'x-api-key'):
                req.headers.pop(header, None)
        token = CURRENT_FLOW.set(flow)
        try:
            decision = await self.control({'action': 'destination', 'request': {
                'host': req.host, 'method': req.method, 'scheme': req.scheme}})
            if not decision.get('allow'):
                await self.deny(flow, 'destination or active lease denied', 403)
                return
            await super().request(flow)
            if not flow.response and req.host in ('api.openai.com', 'chatgpt.com', 'api.anthropic.com'):
                request = {'host': req.host, 'method': req.method,
                           'scheme': req.scheme, 'path': req.path}
                result = await self.control({'action': 'provider',
                    'decision_id': flow.metadata.get('warden_decision_id'), 'request': request})
                if not result.get('allow'):
                    await self.deny(flow, 'provider credential unavailable', 503)
                    return
                headers = result.get('headers', {})
                auth = headers.get('Authorization')
                if not isinstance(auth, str) or not auth.startswith('Bearer '):
                    await self.deny(flow, 'invalid provider authorization', 503)
                    return
                self.redactor.register(auth.removeprefix('Bearer '))
                for key, value in headers.items():
                    if key not in ('Authorization', 'ChatGPT-Account-ID') or not isinstance(value, str):
                        raise ValueError('unsupported credential header')
                    self.redactor.register(value)
                    req.headers[key] = value
        except Exception:
            await self.deny(flow, 'gateway control unavailable', 503)
        finally:
            CURRENT_FLOW.reset(token)

    def tcp_start(self, flow):
        flow.kill()

    def udp_start(self, flow):
        flow.kill()

    async def deny(self, flow, reason, status=403, request_id=None):
        await super().deny(flow, reason, status, request_id)
        flow.request.headers.pop('ChatGPT-Account-ID', None)

    async def error(self, flow):
        await super().error(flow)
        if flow.request: flow.request.headers.pop('ChatGPT-Account-ID', None)

    async def response(self, flow):
        if flow.metadata.get('sbx_health'):
            return  # Liveness must not recursively wait for the broker's lock.
        if flow.metadata.get('sbx_document'):
            return  # The broker has already checked and bounded this response.
        if flow.request.host in ('api.openai.com', 'chatgpt.com', 'api.anthropic.com') and flow.response:
            payload = flow.response.content or b''
            headers = '\n'.join(value for _, value in flow.response.headers.items(multi=True))
            if any(secret.encode() in payload or secret in headers for secret in self.redactor.secrets):
                await self.deny(flow, 'provider response exposed a protected credential', 502)
        try:
            await super().response(flow)
        finally:
            flow.request.headers.pop('ChatGPT-Account-ID', None)


def configured_addon():
    path = os.environ.get('WARDEN_SBX_GATEWAY_CONFIG')
    if not path:
        return [ClosedGateway()]
    config_path = Path(path)
    info = config_path.stat()
    if info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise ValueError('gateway config must be private')
    value = json.loads(config_path.read_text())
    return [SbxGuard(value['socketPath'], value['bindingID'], value['capability'], value['gatewayPort'])]


class ClosedGateway:
    """A missing configuration must never leave a running unguarded proxy."""
    def running(self):
        from mitmproxy import ctx
        ctx.master.shutdown()

    def server_connect(self, data):
        data.server.error = 'gateway is not configured'

    def requestheaders(self, flow):
        from mitmproxy import http
        flow.response = http.Response.make(503, b'Gateway is not configured')

    http_connect = requestheaders


addons = configured_addon()
