"""Guest assets and a fixed one-way text inbox; no arbitrary control RPC."""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import threading
from clipboard import take, acknowledge
FILES=Path('/opt/warden/guest-assets')
clipboard_slot = threading.BoundedSemaphore(1)
class Server(ThreadingHTTPServer):
    daemon_threads = True
    slots = threading.BoundedSemaphore(16)
    def process_request(self, request, address):
        if not self.slots.acquire(blocking=False): self.shutdown_request(request); return
        try: super().process_request(request, address)
        except BaseException: self.slots.release(); raise
    def process_request_thread(self, request, address):
        try: super().process_request_thread(request, address)
        finally: self.slots.release()
class Handler(BaseHTTPRequestHandler):
    def setup(self): super().setup(); self.connection.settimeout(5)
    def log_message(self,*args): pass
    def do_POST(self):
        if self.path != '/clipboard/ack': self.send_error(404); return
        if self.headers.get_all('X-Warden-Clipboard', []) != ['1'] or self.headers.get('Origin') or self.headers.get('Transfer-Encoding') or self.headers.get_all('Content-Length',[]) != ['0']:
            self.send_error(403); return
        ids=self.headers.get_all('X-Warden-Transfer',[])
        if len(ids)!=1 or len(ids[0])!=36: self.send_error(400); return
        try: accepted=acknowledge(ids[0])
        except Exception: self.send_error(503); return
        self.send_response(204 if accepted else 409)
        self.send_header('Cache-Control','no-store')
        self.send_header('Content-Length','0'); self.end_headers()
    def do_GET(self):
        if self.path == '/clipboard':
            # Cross-origin pages cannot consume clipboard transfers. The guest
            # agent supplies this fixed header; browsers require a CORS preflight.
            if self.headers.get_all('X-Warden-Clipboard', []) != ['1'] or self.headers.get('Origin'):
                self.send_error(403); return
            if self.headers.get_all('X-Warden-Clipboard-Version',[]) != ['2']:
                self.send_error(426); return
            if not clipboard_slot.acquire(blocking=False):
                self.send_error(429); return
            try:
                try: transfer = take()
                except Exception: self.send_error(503); return
                data, transfer_id = transfer if transfer is not None else (None, None)
                self.send_response(204 if data is None else 200)
                if transfer_id is not None: self.send_header('X-Warden-Transfer', transfer_id)
                self.send_header('Content-Type', 'text/plain; charset=utf-8')
                self.send_header('Cache-Control', 'no-store')
                self.send_header('X-Content-Type-Options', 'nosniff')
                self.send_header('Content-Length', str(len(data) if data is not None else 0))
                self.end_headers()
                if data is not None: self.wfile.write(data)
            finally: clipboard_slot.release()
            return
        name=self.path.removeprefix('/')
        if name=='ca.pem': path=Path('/var/lib/warden/mitmproxy/mitmproxy-ca-cert.pem')
        elif name and '/' not in name and '?' not in name and name not in ('.','..'): path=FILES/name
        else: self.send_error(404); return
        if not path.is_file() or path.is_symlink(): self.send_error(404); return
        self.send_response(200); self.send_header('Content-Type','application/octet-stream'); self.send_header('Content-Length',str(path.stat().st_size)); self.end_headers()
        with path.open('rb') as f:
            while chunk:=f.read(1024*1024): self.wfile.write(chunk)
if __name__ == '__main__':
    Server(('10.77.0.1',8081),Handler).serve_forever()
