"""Transparent HTTP(S) enforcement in the trusted Linux VM.

No client-configured proxy is trusted. nftables redirects all permitted guest
TCP traffic here and drops everything else. No general raw TCP/WebSocket
passthrough; exact Apple update TLS hosts have a destination-pinned exception.
"""
from __future__ import annotations
import asyncio
import base64
import ipaddress
import json
import os
from pathlib import Path
import socket
import sys
import time
import uuid
import hashlib

from mitmproxy import http, ctx
PAYLOAD = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(PAYLOAD/'host'))
from warden.core import Redactor, github_host, dumps
from warden.figma import protected_host as figma_host
from warden.google_docs import protected_host as google_docs_host
from warden.git_protocol import route as git_route, inspect as inspect_git
sys.path.insert(0, str(PAYLOAD/'proxy'))
from git_review import inspect_push
from response_stream import ResponseStream, StreamRejected, provider_request

STRIP = {'authorization','x-figma-token','x-goog-api-key','x-goog-user-project','proxy-authorization','cookie','host','connection','proxy-connection','transfer-encoding','content-length','keep-alive','upgrade','trailer','te'}
FORBID = {'x-http-method-override','x-method-override','x-original-url','x-rewrite-url','x-forwarded-host','forwarded'}
MAX_MESSAGE=12*1024*1024

# Exact update endpoints from https://support.apple.com/en-us/101555.
# No wildcard, general Apple/iCloud bypass, or guest-controlled tunnel target.
APPLE_UPDATE_TLS = frozenset({
    'configuration.apple.com', 'gdmf.apple.com', 'gdmf-ados.apple.com',
    'gg.apple.com', 'gs.apple.com', 'ig.apple.com', 'mesu.apple.com',
    'oscdn.apple.com', 'osrecovery.apple.com', 'skl.apple.com',
    'swcdn.apple.com', 'swdist.apple.com', 'swdownload.apple.com',
    'swscan.apple.com', 'updates.cdn-apple.com', 'xp.apple.com',
})
APPLE_DOWNLOAD_HTTPS = {
    'swcdn.apple.com': 'swcdn.apple.com',
    'swdownload.apple.com': 'swdownload.apple.com',
    'oscdn.apple.com': 'oscdn.apple.com',
    'osrecovery.apple.com': 'osrecovery.apple.com',
    'updates-http.cdn-apple.com': 'updates.cdn-apple.com',
}

def rpc(message):
    with socket.socket(socket.AF_VSOCK,socket.SOCK_STREAM) as sock:
        sock.settimeout(8); sock.connect((socket.VMADDR_CID_HOST,7000))
        sock.sendall((dumps(message)+'\n').encode())
        result=bytearray()
        while not result.endswith(b'\n'):
            data=sock.recv(65536)
            if not data or len(result)+len(data)>MAX_MESSAGE: raise OSError('invalid host response')
            result.extend(data)
        value=json.loads(result)
        if 'error' in value: raise OSError('host rejected control request')
        return value

async def call(message): return await asyncio.to_thread(rpc,message)

class Guard:
    def __init__(self):
        self.redactor=Redactor(); self.tasks={}; self.networks=[]; self.heartbeat=None; self.apple_tunnels={}
        self.review_slots=asyncio.Semaphore(2); self.review_cache={}
        self.streams = {}
        self.stream_cleanup = set()
        data=json.loads((PAYLOAD/'vendor/github-meta.json').read_text())
        for values in data.values():
            if isinstance(values,list):
                for value in values:
                    try: self.networks.append(ipaddress.ip_network(value))
                    except (ValueError,TypeError): pass
    async def control(self, message):
        return await call(message)

    async def tls_clienthello(self, data):
        host = data.client_hello.sni
        if not isinstance(host, str) or host.lower() not in APPLE_UPDATE_TLS:
            return
        host = host.lower()
        server = data.context.server
        # An early connection would defeat destination pinning. Keep interception
        # in that case; this exception requires lazy connection establishment.
        if server.connected:
            return
        # Set the deny state BEFORE any await. Even an unexpected hook exception
        # cannot let mitmproxy open the guest-selected original destination.
        server.error = 'Apple update destination validation failed'
        data.ignore_connection = True
        try:
            if not server.address or server.address[1] != 443:
                raise ValueError('Apple update TLS requires port 443')
            answers = await asyncio.wait_for(asyncio.get_running_loop().getaddrinfo(
                host, 443, type=socket.SOCK_STREAM), timeout=5)
            addresses = sorted({ipaddress.ip_address(answer[4][0]) for answer in answers}, key=str)
            if not addresses or any(not address.is_global or
                any(address in network for network in self.networks) for address in addresses):
                raise ValueError('Apple update DNS resolved to a private or protected destination')
            upstream = next((str(address) for address in addresses if address.version == 4), None)
            if not upstream:
                raise ValueError('Apple update IPv4 destination unavailable')
            target = (upstream, 443)
            decision=await self.control({'action':'egress','request':{'host':host,'method':'GET','scheme':'https','tls':True}})
            if not decision.get('allow'):raise ValueError('Apple update destination denied by host policy')
            request_id = str(uuid.uuid4())
            await self.control({'action':'event', 'event_type':'tls.passthrough', 'fields':{
                'request_id':request_id, 'hostname':host,
                'reason':'Apple update TLS permitted without decryption; destination pinned by proxy',
                'destination':{'SRC':data.context.client.peername[0], 'DST':upstream, 'DPT':443, 'PROTO':'TCP'}}})
            server.address = target
            self.apple_tunnels[data.context.client.id] = (server.id, target)
            server.error = None
        except Exception as error:
            # No fallback to the original destination or to unaudited forwarding.
            server.error = 'Apple update destination validation or audit failed'
            reason = str(error) if isinstance(error, ValueError) else type(error).__name__
            try:
                await self.control({'action':'event', 'event_type':'network.denied', 'fields':{
                    'hostname':host, 'reason':'Apple update TLS denied: ' + reason}})
            except Exception:
                pass

    def server_connect(self, data):
        tunnel = self.apple_tunnels.get(data.client.id)
        if tunnel and (data.server.id, data.server.address) != tunnel:
            data.server.error = 'Apple update pinned destination changed'

    def client_disconnected(self, client):
        self.apple_tunnels.pop(client.id, None)

    async def running(self):
        await self.control({'action':'event','event_type':'proxy.started','fields':{'reason':'transparent HTTP(S), strict upstream TLS, destination-pinned Apple update TLS exception'}})
        self.heartbeat=asyncio.create_task(self.beat())
    async def beat(self):
        while True:
            try:
                self.review_cache={k:v for k,v in self.review_cache.items() if v[0]>time.monotonic()}
                # A kernel oops invalidates confidence in the enforcement VM.
                if int(Path('/proc/sys/kernel/tainted').read_text()) & 128:
                    ctx.master.shutdown(); return
                # Firewall is loaded before mitmdump starts. Validate the kernel
                # still has it, not just that a marker file exists.
                process=await asyncio.create_subprocess_exec('/usr/sbin/nft','list','table','inet','warden',stdout=asyncio.subprocess.DEVNULL,stderr=asyncio.subprocess.DEVNULL)
                if await process.wait()!=0: ctx.master.shutdown(); return
                result=await self.control({'action':'ready','firewall':'enforced','network_enabled':getattr(self,'network_enabled',None)})
                enabled=result['network_enabled']
                from network_control import apply_network
                await asyncio.to_thread(apply_network,enabled)
                self.network_enabled=enabled
            except Exception:
                from network_control import apply_network
                try:await asyncio.to_thread(apply_network,False);self.network_enabled=False
                except Exception:ctx.master.shutdown();return
            await asyncio.sleep(3)
    def done(self):
        if self.heartbeat: self.heartbeat.cancel()
        for task in self.tasks.values(): task.cancel()
        for task in self.stream_cleanup: task.cancel()
        for state in self.streams.values(): state.clear()
        self.streams.clear()
    async def deny(self,flow,reason,status=403,request_id=None):
        request_id=request_id or flow.metadata.get('warden_request_id') or str(uuid.uuid4())
        flow.metadata['warden_request_id']=request_id
        message={'error':reason,'request_id':request_id}
        if status==428: message['approval']='Use the Warden control panel on the host. Retry after approval.'
        flow.response=http.Response.make(status,dumps(message).encode(),{'Content-Type':'application/json','Cache-Control':'no-store'})
        flow.metadata['warden_denied']=True
        req=flow.request
        if flow.metadata.get('warden_decision_id'): req.headers.pop('Authorization',None)
        try:
            # Record early validation failures too, without retaining credentials
            # or reflecting arbitrary malformed header values into the audit.
            await self.control({'action':'event','event_type':'proxy.error','fields':{
                'request_id':request_id,'status':status,'reason':self.redactor.text(reason),
                'hostname':flow.client_conn.sni,
                'request':{'method':req.method,'host':req.host,
                    'path':self.redactor.text(req.path.split('?')[0]),
                    'query':'[REDACTED]' if '?' in req.path else '',
                    'http_version':req.http_version,
                    'header_names':[self.redactor.text(k) for k,v in req.headers.items(multi=True)]}}})
        except Exception: pass  # The response remains denied when auditing is unavailable.
    async def requestheaders(self,flow):
        # Authorization and inspection must see the complete request.
        flow.request.stream = False
        if flow.request.headers.get('Upgrade') or flow.request.method in ('CONNECT','TRACE'):
            await self.deny(flow,'tunnels and protocol upgrades are unsupported'); return
        try:
            length=int(flow.request.headers.get('Content-Length','0'))
            if length>8388608 or length<0: await self.deny(flow,'request exceeds inspection limit',413)
        except ValueError: await self.deny(flow,'invalid content length')
    async def request(self,flow):
        if flow.metadata.get('warden_denied'): return
        req=flow.request
        try:
            authority=req.host_header or req.headers.get('host','')
            host=authority.split(':',1)[0].lower().rstrip('.')
            # Restrict to canonical DNS names; IP literals, HTTP authority tricks,
            # private routes and client-selected upstreams never get passthrough.
            import re
            if not re.fullmatch(r'(?=.{1,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}',host): raise ValueError('a canonical public DNS hostname is required')
            if req.scheme not in ('http','https') or req.port not in (80,443) or (req.scheme=='https') != (req.port==443): raise ValueError('unsupported transport')
            if authority.lower().rstrip('.') not in (host,host+':'+str(req.port)): raise ValueError('HTTP authority mismatch')
            sni=flow.client_conn.sni
            if req.scheme=='https' and (not sni or sni.lower().rstrip('.')!=host): raise ValueError('TLS SNI and HTTP authority must agree')
            # RFC 9113 section 8.2.3 permits HTTP/2 to split Cookie fields.
            # Recombine with semicolons, never commas; retain all other duplicate
            # checks. GitHub cookies are still stripped before authorization.
            cookies=req.headers.get_all('cookie')
            if req.http_version=='HTTP/2.0' and len(cookies)>1:
                req.headers.set_all('cookie',['; '.join(cookies)])
            pairs=[[k,v] for k,v in req.headers.items(multi=True)]
            seen=set()
            for k,v in pairs:
                key=k.lower()
                if key in FORBID: raise ValueError('HTTP routing overrides forbidden')
                if key in seen: raise ValueError('duplicate headers unsupported')
                seen.add(key)
            results=await asyncio.get_running_loop().getaddrinfo(host,req.port,type=socket.SOCK_STREAM)
            addresses=sorted({result[4][0] for result in results})
            if not addresses or any(not ipaddress.ip_address(a).is_global for a in addresses): raise ValueError('private or special-use destination blocked')
            protected=github_host(host) or figma_host(host) or google_docs_host(host) or any(any(ipaddress.ip_address(a) in n for n in self.networks) for a in addresses)
            # A client can send to any destination IP; upstream always goes to a
            # freshly resolved, validated address for the inspected authority.
            # Pin the address to prevent a second DNS lookup/rebinding race.
            upstream=next((a for a in addresses if ':' not in a),None)
            if not upstream: raise ValueError('IPv6-only upstream unsupported')
            flow.server_conn.address=(upstream,req.port)
            flow.server_conn.sni=host
            req.host=host
            body=req.content or b''
            if len(body)>8388608: raise ValueError('request exceeds inspection limit')
            request_id=str(uuid.uuid4()); flow.metadata['warden_request_id']=request_id
            if protected:
                # All guest credentials are discarded, including hop-by-hop
                # nominated headers. GitHub only sees the brokered identity.
                filtered=[[k,v] for k,v in pairs if k.lower() not in STRIP]
                git = inspect_git(req.method,req.path,filtered,body) if host=='github.com' and git_route(req.method,req.path) else None
                review = None
                if git and git['write']:
                    # Reading upstream objects is separately authorized. A read
                    # session cannot dispatch a receive-pack request.
                    read = await self.control({'action':'authorize','request':{'method':'GET','host':host,
                        'path':'/'+git['repository']+'.git/info/refs?service=git-upload-pack',
                        'scheme':req.scheme,'port':req.port,'headers':[]}})
                    if not read.get('allow'):
                        await self.deny(flow,read.get('reason','repository read permission required'),read.get('status',403),read.get('request_id')); return
                    async def active():
                        return (await self.control({'action':'active','decision_id':read['decision_id']}))['active']
                    cache_key=(git['repository'],hashlib.sha256(body).hexdigest())
                    self.review_cache={k:v for k,v in self.review_cache.items() if v[0]>time.monotonic()}
                    if cache_key in self.review_cache:
                        _,review,body=self.review_cache[cache_key]
                        if not await active(): raise ValueError('repository read permission expired')
                    else:
                        await asyncio.wait_for(self.review_slots.acquire(),timeout=5)
                        try:
                            credential='Basic '+base64.b64encode(('x-access-token:'+read['authorization'].removeprefix('Bearer ')).encode()).decode()
                            review,body=await inspect_push(git['repository'],body,credential,upstream,active)
                            if len(body)>8388608: raise ValueError('canonical push exceeds 8 MiB; split the change')
                            if len(self.review_cache)>=4: self.review_cache.pop(next(iter(self.review_cache)))
                            self.review_cache[cache_key]=(time.monotonic()+120,review,body)
                        finally: self.review_slots.release()
                request={'method':req.method,'host':host,'path':req.path,'scheme':req.scheme,'port':req.port,'headers':filtered,'body_base64':base64.b64encode(body).decode()}
                if review: request['git_review']=review
                decision=await self.control({'action':'authorize','request':request})
                flow.metadata['warden_request_id']=decision.get('request_id',request_id)
                if not decision.get('allow'):
                    await self.deny(flow,decision.get('reason','denied'),decision.get('status',403),decision.get('request_id')); return
                # Upstream destination is fixed to a supported GitHub authority;
                # no redirects are followed and no alternate-host injection.
                for key in list(req.headers.keys()):
                    if key.lower() in STRIP: del req.headers[key]
                req.host_header=host
                req.headers['Authorization']=('Basic '+base64.b64encode(('x-access-token:'+decision['authorization'].removeprefix('Bearer ')).encode()).decode()) if git else decision['authorization']
                self.redactor.register(req.headers['Authorization'])
                self.redactor.register(decision['authorization'].split(' ',1)[1])
                if 'User-Agent' not in req.headers: req.headers['User-Agent']='Warden/0.1'
                req.content=body
                # Figma's load balancer rejects GET with Content-Length: 0.
                # The Figma adapter already rejects all nonempty GET bodies.
                if host in ('api.figma.com', 'docs.googleapis.com') and req.method == 'GET' and not body:
                    req.headers.pop('Content-Length', None)
                flow.metadata['warden_git']=bool(git)
                flow.metadata['warden_decision_id']=decision['decision_id']
                active=await self.control({'action':'active','decision_id':decision['decision_id']})
                if not active['active']: await self.deny(flow,'permission expired before dispatch'); return
                self.tasks[flow.id]=asyncio.create_task(self.watch(flow,decision))
            else:
                upgrade=req.scheme=='http' and host in APPLE_DOWNLOAD_HTTPS and req.method in ('GET','HEAD')
                decision=await self.control({'action':'egress','request':{'host':APPLE_DOWNLOAD_HTTPS[host] if upgrade else host,'method':req.method,'scheme':'https' if upgrade else req.scheme}})
                if not decision.get('allow'):
                    await self.deny(flow,decision.get('reason','destination denied'),403);return
                flow.metadata['warden_decision_id']=decision['decision_id']
                self.tasks[flow.id]=asyncio.create_task(self.watch(flow,decision))
                # Other sites retain their credentials for ordinary application
                # login, but logs contain only their scrubbed representations.
                summary={'method':req.method,'host':host,'path':self.redactor.text(req.path.split('?')[0]),'query':'[REDACTED]' if '?' in req.path else '', 'headers':self.redactor.headers(pairs),'body':self.redactor.body(body,req.headers.get('Content-Type',''))}
                await self.control({'action':'event','event_type':'http.request.external','fields':{'request_id':request_id,'request':summary}})
                # Apple also advertises cleartext package URLs. Upgrade these
                # exact download hosts to their supported end-to-end TLS route;
                # multi-GB installers then avoid the inspected-body size cap.
                if req.scheme == 'http' and host in APPLE_DOWNLOAD_HTTPS and req.method in ('GET', 'HEAD'):
                    flow.response = http.Response.make(307, b'', {
                        'Location':'https://' + APPLE_DOWNLOAD_HTTPS[host] + req.path,
                        'Cache-Control':'no-store'})
            if not flow.response and provider_request(req):
                req.headers['Accept-Encoding'] = 'identity'
        except Exception as error:
            # Fail closed on unavailable policy/audit service or malformed input.
            reason=str(error) if isinstance(error,ValueError) else 'inspection or control plane unavailable'
            await self.deny(flow,reason,503 if not isinstance(error,ValueError) else 403)
    async def watch(self,flow,decision):
        deadline=time.monotonic()+min(decision['remaining_seconds'],3600)
        try:
            while True:
                await asyncio.sleep(min(.5,max(.01,deadline-time.monotonic())))
                if time.monotonic()>=deadline or not (await self.control({'action':'active','decision_id':decision['decision_id']}))['active']:
                    if flow.id in self.streams:
                        self.abort_stream(flow, 'permission expired or revoked')
                        return
                    flow.kill()
                    await self.control({'action':'event','event_type':'request.interrupted','fields':{'request_id':decision['request_id'],'decision_id':decision['decision_id'],'reason':'permission expired or revoked'}})
                    return
        except asyncio.CancelledError: pass
        except Exception:
            if flow.id in self.streams:
                self.abort_stream(flow, 'permission verification unavailable')
            elif flow.killable:
                flow.kill()
    async def responseheaders(self,flow):
        flow.response.stream = False
        if flow.metadata.get('warden_denied') or flow.metadata.get('sbx_health'): return
        response = flow.response
        content_type = response.headers.get('Content-Type', '').split(';', 1)[0].strip().lower()
        # The Codex backend can omit Content-Type on streaming responses.
        # Restrict inference to that exact route and an explicit JSON stream flag.
        codex_stream = False
        if (not content_type and provider_request(flow.request)
                and flow.request.host == 'chatgpt.com'):
            try:
                payload = json.loads(flow.request.content or b'')
                codex_stream = isinstance(payload, dict) and payload.get('stream') is True
            except (ValueError, UnicodeError):
                pass
        candidate = (provider_request(flow.request) and 200 <= response.status_code < 300
                     and (content_type == 'text/event-stream' or codex_stream))
        async def reject(reason, status=403):
            await self.deny(flow, reason, status)
            # Replacing a response in responseheaders does not stop mitmproxy
            # consuming the upstream body. Kill rejected SSE flows at this hook
            # so an endless upstream cannot delay denial until EOF.
            if candidate and flow.killable: flow.kill()
        limit=128*1024*1024 if flow.metadata.get('warden_git') else 16*1024*1024
        try:
            length = int(flow.response.headers.get('Content-Length','0'))
            if length < 0: raise ValueError('negative response length')
            if length > limit:
                await reject('response exceeds inspection limit',413)
                return
        except ValueError:
            await reject('invalid response length')
            return
        if not candidate:
            return
        state = None
        try:
            decision = flow.metadata.get('warden_decision_id')
            if not decision or not (await self.control({'action':'active', 'decision_id':decision})).get('active'):
                await reject('permission expired before response delivery')
                return
            # Capture the known secrets before any body bytes or headers leave.
            state = ResponseStream(self.redactor.secrets,
                                   response.headers.get('Content-Encoding', 'identity').strip().lower())
            headers = bytes(response.headers)
            if any(secret in headers for secret in state.secrets):
                raise StreamRejected('provider response exposed a protected credential')
            if response.headers.get('Trailer'):
                raise StreamRejected('response stream trailers unsupported')
            response.headers['Alt-Svc'] = 'clear'
            # The callback returns inspected, decoded bytes. Let mitmproxy choose
            # HTTP/1 chunk framing or HTTP/2 DATA frames for the downstream side.
            response.headers.pop('Content-Encoding', None)
            response.headers.pop('Content-Length', None)
            response.headers.pop('Trailer', None)
            result = await self.control({'action':'event', 'event_type':'http.response.started',
                'fields':self.stream_fields(flow, state, 'started')})
            if not result.get('recorded'):
                raise OSError('audit rejected')
            # A slow audit must not allow an expired lease to begin streaming.
            if flow.error or not (await self.control({'action':'active', 'decision_id':decision})).get('active'):
                await reject('permission expired before response delivery')
                state.clear()
                return
            self.streams[flow.id] = state
            flow.metadata['warden_stream'] = True
            response.stream = lambda chunk: self.stream_chunk(flow, state, chunk)
        except Exception as error:
            if state: state.clear()
            await reject(str(error) if isinstance(error, StreamRejected)
                         else 'stream inspection or audit unavailable', 502 if isinstance(error, StreamRejected) else 503)

    def stream_fields(self, flow, state, phase):
        return {'request_id':flow.metadata.get('warden_request_id'),
                'decision_id':flow.metadata.get('warden_decision_id'),
                'status':flow.response.status_code,
                'response':{'headers':self.redactor.headers(list(flow.response.headers.items(multi=True))),
                            'body':{'bytes':state.decoded, 'capture':'omitted_policy'},
                            'stream':state.summary(phase)}}

    def stream_chunk(self, flow, state, chunk):
        if state.reason or flow.error:
            return []
        try:
            # An empty DATA event becomes a zero-length HTTP/1 chunk, which
            # would falsely signal successful EOF. Suppress the event entirely.
            output = state.feed(chunk)
            return output if output else []
        except Exception as error:
            self.abort_stream(flow, str(error) if isinstance(error, StreamRejected)
                              else 'response stream inspection failed')
            return []

    def close_stream_transport(self, flow):
        if flow.killable: flow.kill()
        # Pinned mitmproxy 12.2.3 only observes flow.kill() at hook boundaries,
        # not during streaming. Close the client transport to stop idle streams
        # immediately too. This also closes sibling HTTP/2 streams on that client.
        master = getattr(ctx, 'master', None)
        if master:
            proxyserver = master.addons.get('proxyserver')
            handler = proxyserver.connections.get(flow.client_conn.id) if proxyserver else None
            if handler and handler.transports.get(flow.client_conn) and handler.transports[flow.client_conn].handler:
                handler.close_connection(flow.client_conn)

    def abort_stream(self, flow, reason):
        state = self.streams.get(flow.id)
        if not state or state.reason: return
        state.reason = reason
        state.clear()
        try:
            self.close_stream_transport(flow)
        finally:
            task = asyncio.create_task(self.finish_stream(flow))
            self.stream_cleanup.add(task)
            task.add_done_callback(self.stream_cleanup.discard)

    async def finish_stream(self, flow):
        state = self.streams.pop(flow.id, None)
        if not state: return
        task = self.tasks.pop(flow.id, None)
        if task: task.cancel()
        try:
            # Trailers are held until this hook. Never forward uninspected fields.
            flow.response.trailers = None
            if not state.reason:
                # Header-only responses have no streaming callback, even at EOF.
                if not flow.error and not state.received and not state.finished:
                    try: state.feed(b'')
                    except StreamRejected as error: state.reason = str(error)
                decision = flow.metadata.get('warden_decision_id')
                if flow.error or not state.finished:
                    state.reason = 'response stream interrupted'
                elif not (await self.control({'action':'active', 'decision_id':decision})).get('active'):
                    state.reason = 'permission expired or revoked'
            if state.reason: self.close_stream_transport(flow)
            fields = self.stream_fields(flow, state, 'interrupted' if state.reason else 'completed')
            if state.reason: fields['reason'] = state.reason
            result = await self.control({'action':'event',
                'event_type':'request.interrupted' if state.reason else 'http.response', 'fields':fields})
            if not result.get('recorded'): raise OSError('audit rejected')
        except Exception:
            self.close_stream_transport(flow)
        finally:
            state.clear()
            # Do not retain a closure referencing credentials on completed flows.
            flow.response.stream = lambda _: []
            flow.request.headers.pop('Authorization', None)
            flow.request.headers.pop('ChatGPT-Account-ID', None)
            try: await self.control({'action':'egress.finish', 'decision_id':flow.metadata.get('warden_decision_id')})
            except Exception: pass

    async def response(self,flow):
        if flow.metadata.get('warden_stream'):
            await self.finish_stream(flow)
            return
        task=self.tasks.pop(flow.id,None)
        if task: task.cancel()
        if flow.metadata.get('warden_denied'):
            try:await self.control({'action':'egress.finish','decision_id':flow.metadata.get('warden_decision_id')})
            except Exception:pass
            return
        try:
            decision=flow.metadata.get('warden_decision_id')
            if decision and not (await self.control({'action':'active','decision_id':decision}))['active']:
                await self.deny(flow,'permission expired before response delivery'); return
            response=flow.response
            if len(response.content or b'')>(128*1024*1024 if flow.metadata.get('warden_git') else 16*1024*1024):
                await self.deny(flow,'response exceeds inspection limit',413); return
            # This appliance carries HTTPS over TCP. Clear origin advertisements
            # for QUIC/alternate ports, including ones already cached by clients.
            response.headers['Alt-Svc']='clear'
            await self.control({'action':'event','event_type':'http.response','fields':{'request_id':flow.metadata.get('warden_request_id'),'decision_id':decision,'status':response.status_code,'response':{'headers':self.redactor.headers(list(response.headers.items(multi=True))),'body':self.redactor.body(response.content or b'',response.headers.get('Content-Type',''))}}})
        except Exception: await self.deny(flow,'audit unavailable; response withheld',503)
        finally:
            try:await self.control({'action':'egress.finish','decision_id':flow.metadata.get('warden_decision_id')})
            except Exception:pass
            # Never leave the host credential in retained mitmproxy flow objects.
            if flow.metadata.get('warden_decision_id'): flow.request.headers.pop('Authorization',None)
    async def error(self,flow):
        if flow.metadata.get('warden_stream'):
            await self.finish_stream(flow)
            return
        task=self.tasks.pop(flow.id,None)
        if task: task.cancel()
        flow.request.headers.pop('Authorization',None) if flow.request else None
        try: await self.control({'action':'event','event_type':'proxy.error','fields':{'request_id':flow.metadata.get('warden_request_id'),'hostname':flow.client_conn.sni,'reason':'upstream or transport error: '+self.redactor.text(str(flow.error.msg if flow.error else 'unknown'))[:1024]}})
        except Exception: pass
        try:await self.control({'action':'egress.finish','decision_id':flow.metadata.get('warden_decision_id')})
        except Exception:pass

    async def tls_failed_client(self,data):
        await self.tls_failure(data,'client')

    async def tls_failed_server(self,data):
        await self.tls_failure(data,'upstream')

    async def tls_failure(self,data,side):
        try:
            await self.control({'action':'event','event_type':'proxy.error','fields':{'hostname':data.conn.sni,'reason':'TLS '+side+' handshake failed: '+self.redactor.text(str(data.conn.error or 'unknown'))[:1024]}})
        except Exception: pass

addons=[Guard()]
