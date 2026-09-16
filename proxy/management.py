"""Trusted host maintenance channel. No network listener; host CID 2 only."""
import json
import socket
import subprocess
import threading
import ipaddress
from pathlib import Path
import select
import time

GUEST_MAC='02:57:41:00:00:03'
GUEST_PORT=2222

def guest_address(leases='/var/lib/misc/dnsmasq.leases', now=None):
    now=time.time() if now is None else now
    matches=[]
    for line in Path(leases).read_text().splitlines():
        parts=line.split()
        if len(parts)>=3 and parts[1].lower()==GUEST_MAC:
            address=ipaddress.IPv4Address(parts[2])
            if address not in ipaddress.IPv4Network('10.77.0.0/24') or int(str(address).split('.')[-1]) not in range(10,101):
                raise ValueError('invalid guest address')
            if int(parts[0]) and int(parts[0])<=now: raise ValueError('guest lease expired')
            matches.append(str(address))
    if len(matches)!=1: raise ValueError('guest lease unavailable or ambiguous')
    return matches[0]

def relay(left,right):
    # Fixed destination only; no guest-selected address, listener, or RPC.
    left.settimeout(30);right.settimeout(30)
    while True:
        ready,_,_=select.select([left,right],[],[],120)
        if not ready:return
        for source in ready:
            data=source.recv(65536)
            if not data:return
            (right if source is left else left).sendall(data)

def handle(conn):
    with conn:
        conn.settimeout(60)
        try:
            # No buffered read-ahead: a stream handshake must leave every SSH
            # byte on the socket for relay(). Host waits for the acknowledgement.
            line=bytearray()
            while not line.endswith(b'\n') and len(line)<=65536:
                byte=conn.recv(1)
                if not byte:raise ValueError('incomplete command')
                line.extend(byte)
            if len(line)>65536: raise ValueError('oversized command')
            message=json.loads(line)
            if message == {'action':'guest.stream'}:
                with socket.create_connection((guest_address(),GUEST_PORT),timeout=10) as guest:
                    conn.sendall(b'{"ready":true}\n')
                    try:relay(conn,guest)
                    except OSError:pass
                return
            if message == {'action':'guest.keyscan'}:
                address=guest_address()
                result=subprocess.run(['ssh-keyscan','-T','5','-p',str(GUEST_PORT),'-t','ed25519',address],capture_output=True,timeout=8)
                keys={line.split()[1]+' '+line.split()[2] for line in result.stdout.decode('ascii').splitlines() if not line.startswith('#') and len(line.split())==3 and line.split()[1]=='ssh-ed25519'}
                if len(keys)!=1:raise ValueError('guest SSH key unavailable')
                conn.sendall((json.dumps({'key':keys.pop()})+'\n').encode());return
            argv=message['argv']
            if not isinstance(argv,list) or not argv or not all(isinstance(a,str) for a in argv): raise ValueError('invalid argv')
            result=subprocess.run(argv,capture_output=True,timeout=min(int(message.get('timeout',30)),180))
            response={'exit_code':result.returncode,'stdout':result.stdout[-1024*1024:].decode(errors='replace'),'stderr':result.stderr[-1024*1024:].decode(errors='replace')}
        except Exception as error: response={'error':type(error).__name__,'errno':getattr(error,'errno',None)}
        conn.sendall((json.dumps(response)+'\n').encode())

def main():
    slots=threading.BoundedSemaphore(16)
    def worker(conn):
        try:handle(conn)
        finally:slots.release()
    listener=socket.socket(socket.AF_VSOCK,socket.SOCK_STREAM); listener.bind((socket.VMADDR_CID_ANY,7001)); listener.listen(8)
    print('Warden trusted host maintenance ready',flush=True)
    while True:
        conn,peer=listener.accept()
        if peer[0]!=socket.VMADDR_CID_HOST or not slots.acquire(blocking=False):conn.close();continue
        threading.Thread(target=worker,args=(conn,),daemon=True).start()

if __name__=='__main__':main()
