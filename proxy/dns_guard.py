"""Gate guest DNS before recursive resolution, closing arbitrary-name egress."""
import json
import os
import socket
import socketserver
import struct
import threading


def question(packet):
    if len(packet)<17 or len(packet)>4096:raise ValueError('invalid DNS length')
    ident,flags,qd,an,ns,ar=struct.unpack('!6H',packet[:12])
    if flags & 0xf800 or qd!=1 or an or ns:raise ValueError('query required')
    offset=12;labels=[]
    while True:
        length=packet[offset];offset+=1
        if length==0:break
        if length>63 or offset+length>=len(packet):raise ValueError('invalid label')
        labels.append(packet[offset:offset+length].decode('ascii').lower());offset+=length
        if len(labels)>127:raise ValueError('invalid name')
    kind,klass=struct.unpack('!HH',packet[offset:offset+4])
    if klass!=1 or kind not in (1,28):raise ValueError('address queries only')
    return '.'.join(labels),offset+4


def permitted(name):
    with socket.socket(socket.AF_VSOCK,socket.SOCK_STREAM) as conn:
        conn.settimeout(3);conn.connect((socket.VMADDR_CID_HOST,7000))
        conn.sendall((json.dumps({'action':'dns','hostname':name})+'\n').encode())
        data=bytearray()
        while not data.endswith(b'\n'):
            chunk=conn.recv(256)
            if not chunk or len(data)>1024:raise ValueError('invalid control response')
            data.extend(chunk)
        return json.loads(data).get('allow') is True


def resolve(packet):
    end=None
    try:
        name,end=question(packet)
        if not permitted(name):raise ValueError('destination denied')
        upstream_id=os.urandom(2)
        canonical=upstream_id+struct.pack('!5H',0x0100,1,0,0,0)+b''.join(bytes([len(label)])+label.encode('ascii') for label in name.split('.'))+b'\0'+packet[end-4:end]
        with socket.socket(socket.AF_INET,socket.SOCK_DGRAM) as upstream:
            upstream.settimeout(4);upstream.connect(('1.1.1.1',53));upstream.send(canonical)
            response=upstream.recv(4096)
            if response[:2]!=upstream_id:raise ValueError('DNS response mismatch')
            return packet[:2]+response[2:]
    except Exception:
        # REFUSED with no copied attacker-controlled records.
        return packet[:2]+struct.pack('!5H',0x8185,1 if end else 0,0,0,0)+(packet[12:end] if end else b'') if len(packet)>=2 else b''


class Server(socketserver.ThreadingUDPServer):
    daemon_threads=True
    slots=threading.BoundedSemaphore(32)
    def process_request(self,request,address):
        if not self.slots.acquire(False):return
        try:super().process_request(request,address)
        except BaseException:self.slots.release();raise
    def process_request_thread(self,request,address):
        try:super().process_request_thread(request,address)
        finally:self.slots.release()
class Handler(socketserver.BaseRequestHandler):
    def handle(self):
        packet,conn=self.request;response=resolve(packet)
        if response:conn.sendto(response,self.client_address)
if __name__=='__main__':
    with Server(('127.0.0.1',5354),Handler) as server:server.serve_forever()
