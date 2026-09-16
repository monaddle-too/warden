from __future__ import annotations
import argparse
import base64
import fcntl
import hmac
import json
import os
from pathlib import Path
import secrets
import socketserver
import threading
import time
from urllib.parse import urlsplit, parse_qsl
from .pages import parameters, history_page, traffic_page, StaleCursor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from .core import Engine, ROOT, dumps, strict_json

MAX_MESSAGE = 12*1024*1024

class LimitedThreads:
    daemon_threads = True
    slots = threading.BoundedSemaphore(64)
    def process_request(self, request, client_address):
        if not self.slots.acquire(blocking=False): self.shutdown_request(request); return
        try: super().process_request(request, client_address)
        except BaseException: self.slots.release(); raise
    def process_request_thread(self, request, client_address):
        try: super().process_request_thread(request, client_address)
        finally: self.slots.release()

class HTTPServer(LimitedThreads, ThreadingHTTPServer): pass
class UnixServer(LimitedThreads, socketserver.ThreadingMixIn, socketserver.UnixStreamServer): pass

def internal(engine, message):
    action = message.get('action')
    if action == 'clipboard.take' and set(message) == {'action'}:
        return engine.clipboard.take()
    if action == 'clipboard.ack' and set(message) == {'action','id'}:
        return engine.clipboard.acknowledge(message['id'])
    if action == 'authorize': return engine.authorize(message['request'])
    if action == 'egress': return engine.authorize_egress(message['request'])
    if action == 'dns':
        from .egress import HOST
        name=message.get('hostname','')
        with engine.lock:
            policy=engine.policy.get('egress',{'mode':'public','destinations':[]})
            allowed=engine.network_enabled and bool(HOST.fullmatch(name)) and (policy['mode']=='public' or name in {'github.com','api.github.com','api.figma.com','docs.googleapis.com'} or name in {r['host'] for r in policy['destinations']})
            if allowed:engine.audit.emit('dns.query',hostname=name,reason='Allowed by destination policy before DNS resolution')
        return {'allow':allowed}
    if action == 'egress.finish':
        with engine.lock:engine.network_decisions.pop(message.get('decision_id'),None)
        return {'released':True}
    if action == 'active': return {'active': engine.active(message['decision_id'])}
    if action == 'event':
        event_type = message.get('event_type')
        if event_type not in ('http.response', 'http.response.started', 'http.request.external', 'network.denied', 'request.interrupted', 'proxy.error', 'dns.query', 'proxy.started', 'tls.passthrough'): raise ValueError('invalid proxy event type')
        fields = message.get('fields', {})
        if not isinstance(fields, dict) or set(fields)-{'request_id','decision_id','status','reason','request','response','destination','hostname','latency_ms'}: raise ValueError('invalid proxy event fields')
        engine.audit.emit(event_type, **fields)
        return {'recorded':True}
    if action == 'ready':
        # Only the trusted proxy can call this socket. Readiness expires without
        # heartbeats; the untrusted Mac is never given a Virtio socket device.
        stamp = {'time':time.time(), 'proxy': 'ready', 'firewall':message.get('firewall'), 'network_enabled':message.get('network_enabled')}
        if stamp['firewall'] != 'enforced': raise ValueError('firewall not ready')
        tmp = engine.state/'proxy-ready.tmp'; tmp.write_text(dumps(stamp)); os.replace(tmp, engine.state/'proxy-ready.json')
        return {'ready':True,'network_enabled':engine.network_enabled}
    raise ValueError('unknown action')

def make_internal_handler(engine):
    class Handler(socketserver.StreamRequestHandler):
        def handle(self):
            self.connection.settimeout(10)
            try:
                line = self.rfile.readline(MAX_MESSAGE+1)
                if len(line)>MAX_MESSAGE or not line.endswith(b'\n'): raise ValueError('oversized control request')
                result = internal(engine, strict_json(line))
            except Exception:
                # Never serialize arbitrary exception strings or incoming bodies.
                result = {'error':'control request rejected', 'allow':False}
            self.wfile.write((dumps(result)+'\n').encode())
    return Handler

def make_handler(engine, admin_token, port):
    allowed_host = '127.0.0.1:'+str(port)
    origin = 'http://'+allowed_host
    class Handler(BaseHTTPRequestHandler):
        server_version = 'Warden/0.1'
        def setup(self): super().setup(); self.connection.settimeout(10)
        def log_message(self, *args): pass
        def send(self, code, value, content_type='application/json'):
            data = dumps(value).encode() if content_type=='application/json' else value
            self.send_response(code)
            self.send_header('Content-Type',content_type)
            self.send_header('Content-Length',str(len(data)))
            self.send_header('Cache-Control','no-store')
            self.send_header('X-Content-Type-Options','nosniff')
            self.send_header('Referrer-Policy','no-referrer')
            self.send_header('Content-Security-Policy',"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
            self.end_headers(); self.wfile.write(data)
        def valid_host(self):
            return self.headers.get_all('Host',[]) == [allowed_host]
        def authenticated(self):
            value = self.headers.get('Authorization','')
            return hmac.compare_digest(value,'Bearer '+admin_token)
        def do_GET(self):
            if not self.valid_host(): return self.send(403,{'error':'invalid host'})
            assets = {'/':('index.html','text/html; charset=utf-8'), '/app.js':('app.js','text/javascript; charset=utf-8'), '/style.css':('style.css','text/css; charset=utf-8')}
            if self.path in assets:
                name,mime = assets[self.path]; return self.send(200,(ROOT/'host/web'/name).read_bytes(),mime)
            if urlsplit(self.path).path == '/oauth/figma/callback':
                try:
                    pairs = parse_qsl(urlsplit(self.path).query, keep_blank_values=True, strict_parsing=True)
                    params = dict(pairs)
                    if len(pairs) != 2 or set(params) != {'state','code'}: raise ValueError('invalid callback')
                    engine.complete_figma(params['state'], params['code'])
                    return self.send(200, b'Figma connected. Return to the Warden tab to approve agent requests.', 'text/plain; charset=utf-8')
                except Exception:
                    return self.send(400, b'Figma connection failed or expired. Return to Warden and try again.', 'text/plain; charset=utf-8')
            if urlsplit(self.path).path == '/oauth/google_docs/callback':
                try:
                    pairs = parse_qsl(urlsplit(self.path).query, keep_blank_values=True, strict_parsing=True)
                    params = dict(pairs)
                    if len(pairs) != len(params) or not {'state','code'} <= set(params) or set(params) - {'state','code','scope','authuser','prompt','iss'}: raise ValueError('invalid callback')
                    if 'iss' in params and params['iss'] != 'https://accounts.google.com': raise ValueError('invalid Google issuer')
                    engine.complete_google_docs(params['state'], params['code'])
                    return self.send(200, b'Google Docs connected. Return to the Warden tab to approve agent requests.', 'text/plain; charset=utf-8')
                except Exception:
                    return self.send(400, b'Google Docs connection failed or expired. Return to Warden and try again.', 'text/plain; charset=utf-8')
            if not self.authenticated(): return self.send(401,{'error':'open the host launch link to authenticate'})
            route=urlsplit(self.path)
            if route.path in ('/api/history','/api/traffic'):
                try:
                    allowed={'cursor','q','status'} if route.path=='/api/history' else {'cursor','q','type'}
                    params=parameters(route.query,allowed)
                    page=history_page(engine,params) if route.path=='/api/history' else traffic_page(engine,params)
                    return self.send(200,page)
                except StaleCursor as error: return self.send(409,{'error':str(error)})
                except (ValueError,TypeError): return self.send(400,{'error':'invalid page query or cursor'})
                except (OSError,RuntimeError): return self.send(503,{'error':'page unavailable; retry shortly'})
            if self.path == '/api/state':
                state = engine.snapshot()
                monitor = getattr(engine, 'resources', None)
                state['resources'] = monitor.snapshot() if monitor else {name: {'status': 'unavailable'} for name in ('macos', 'proxy')}
                try:
                    ready = json.loads((engine.state/'proxy-ready.json').read_text()); state['proxy_ready'] = 0<=time.time()-ready['time']<15
                    state['network_applied']=ready.get('network_enabled') if state['proxy_ready'] else None
                except (OSError,ValueError): state['proxy_ready']=False
                return self.send(200,state)
            if self.path == '/api/readiness':
                from .operations import readiness
                return self.send(200,readiness(engine.state))
            if route.path.startswith('/api/request/'):
                request_id=route.path.removeprefix('/api/request/')
                with engine.lock:row=engine.db.execute('SELECT * FROM requests WHERE id=?',(request_id,)).fetchone()
                if not row:return self.send(404,{'error':'request not found'})
                value=dict(row);value['summary']=json.loads(value['summary']);value.pop('fingerprint',None)
                return self.send(200,engine.redactor.clean(value))
            return self.send(404,{'error':'not found'})
        def do_POST(self):
            if not self.valid_host() or self.headers.get('Origin') != origin or not self.authenticated(): return self.send(403,{'error':'host authentication required'})
            if self.headers.get('Content-Type') != 'application/json' or self.headers.get('Transfer-Encoding'): return self.send(415,{'error':'JSON required'})
            try:
                lengths=self.headers.get_all('Content-Length',[])
                if len(lengths)!=1: raise ValueError('content length required')
                length=int(lengths[0])
                limit = 524288 if self.path == '/api/clipboard' else 262144
                if not 0<=length<=limit: raise ValueError('request too large')
                value=strict_json(self.rfile.read(length))
                if not isinstance(value,dict): raise ValueError('JSON object required')
                if self.path == '/api/token': engine.set_token(value['token']); result={'configured':True}
                elif self.path == '/api/figma/configure':
                    if set(value) != {'client_id','client_secret'}: raise ValueError('unexpected fields')
                    engine.configure_figma(value['client_id'],value['client_secret'],origin+'/oauth/figma/callback'); result={'configured':True}
                elif self.path == '/api/figma/connect': result=engine.connect_figma()
                elif self.path == '/api/figma/disconnect': engine.disconnect_figma(); result={'disconnected':True}
                elif self.path == '/api/google_docs/configure':
                    if set(value) != {'client_id','client_secret'}: raise ValueError('unexpected fields')
                    engine.configure_google_docs(value['client_id'],value['client_secret'],origin+'/oauth/google_docs/callback'); result={'configured':True}
                elif self.path == '/api/google_docs/connect': result=engine.connect_google_docs()
                elif self.path == '/api/google_docs/disconnect': engine.disconnect_google_docs(); result={'disconnected':True}
                elif self.path == '/api/approve': result=engine.approve(value['request_id'],value['kind'],value['ttl'],value.get('predicates'))
                elif self.path == '/api/deny': engine.deny(value['request_id']); result={'denied':True}
                elif self.path == '/api/revoke': engine.revoke(value['grant_id']); result={'revoked':True}
                elif self.path == '/api/revoke-all': engine.revoke_all(); result={'revoked':True}
                elif self.path == '/api/network': engine.set_network(value['enabled']); result={'enabled':engine.network_enabled,'applied':'waiting for appliance acknowledgement'}
                elif self.path == '/api/git-review': result=engine.git_review(value['request_id'])
                elif self.path == '/api/request-review': result=engine.request_review(value['request_id'])
                elif self.path == '/api/policy': engine.save_policy(value); result={'saved':True}
                elif self.path == '/api/clipboard': result=engine.clipboard.send(value['text'],value.get('id'))
                elif self.path == '/api/clipboard/status': result=engine.clipboard.snapshot(value['id'])
                elif self.path == '/api/clipboard/cancel': result=engine.clipboard.cancel(value.get('id'))
                else: return self.send(404,{'error':'not found'})
                self.send(200,result)
            except (KeyError,ValueError,TypeError): self.send(400,{'error':'invalid request; check fields and duration limits'})
            except Exception: self.send(503,{'error':'operation failed closed; check host storage and service'})
    return Handler

def main():
    parser=argparse.ArgumentParser(); parser.add_argument('--state',default=str(ROOT/'.local')); parser.add_argument('--port',type=int,default=18765)
    args=parser.parse_args(); state=Path(args.state).resolve(); state.mkdir(parents=True,exist_ok=True,mode=0o700)
    lock=open(state/'control.lock','a'); fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
    os.umask(0o077)
    engine=Engine(state)
    from .metrics import ResourceMonitor
    engine.resources = ResourceMonitor(state)
    engine.resources.start()
    def expire_previews():
        while True:
            time.sleep(5)
            engine.prune_reviews()
    threading.Thread(target=expire_previews,daemon=True).start()
    admin_token=secrets.token_urlsafe(32); engine.redactor.register(admin_token)
    (state/'admin-token').write_text(admin_token)
    socket_path=state/'control.sock'
    if len(str(socket_path).encode())>100: raise SystemExit('state path too long for a Unix socket')
    socket_path.unlink(missing_ok=True); (state/'proxy-ready.json').unlink(missing_ok=True)
    ipc=UnixServer(str(socket_path),make_internal_handler(engine)); os.chmod(socket_path,0o600)
    threading.Thread(target=ipc.serve_forever,daemon=True).start()
    http=HTTPServer(('127.0.0.1',args.port),make_handler(engine,admin_token,args.port))
    (state/'control-port').write_text(str(args.port))
    print('Warden control plane: http://127.0.0.1:'+str(args.port)+' (use ./warden open for authenticated access)',flush=True)
    try: http.serve_forever()
    finally: ipc.shutdown(); socket_path.unlink(missing_ok=True)

if __name__=='__main__': main()
