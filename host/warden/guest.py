"""Host-initiated SSH to the untrusted guest, through the trusted Linux VM."""
import argparse
import base64
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import socket
import subprocess
import sys
import threading
import time
import uuid

ROOT=Path(__file__).resolve().parents[2]

def key_fingerprint(key):
    parts=key.split()
    if len(parts)!=2 or parts[0]!='ssh-ed25519':raise ValueError('expected one Ed25519 host key')
    blob=base64.b64decode(parts[1],validate=True)
    if len(blob)!=51 or not blob.startswith(b'\0\0\0\x0bssh-ed25519\0\0\0\x20'):raise ValueError('invalid Ed25519 key')
    return 'SHA256:'+base64.b64encode(hashlib.sha256(blob).digest()).decode().rstrip('=')

def initialize(state):
    directory=state/'guest';directory.mkdir(parents=True,exist_ok=True,mode=0o700);directory.chmod(0o700)
    key=directory/'client_key'
    with (directory/'init.lock').open('a') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX)
        if not key.exists():subprocess.run(['/usr/bin/ssh-keygen','-q','-t','ed25519','-N','','-C','warden-guest-access','-f',str(key)],check=True)
        key.chmod(0o600)
        public=' '.join(subprocess.check_output(['/usr/bin/ssh-keygen','-y','-f',str(key)],text=True).split()[:2])
        key_fingerprint(public)
        (directory/'client_key.pub').write_text(public+'\n')
    return directory

def connect(state,action):
    conn=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM);conn.settimeout(15)
    try:
        conn.connect(str(state/'vm-management.sock'));conn.sendall((json.dumps({'action':action})+'\n').encode())
        data=bytearray()
        while not data.endswith(b'\n'):
            byte=conn.recv(1)
            if not byte or len(data)>4096:raise ValueError('invalid maintenance response')
            data.extend(byte)
        value=json.loads(data)
        if 'error' in value:raise ValueError('guest service unavailable; check installation and the VM lease')
        return conn,value
    except BaseException:conn.close();raise

def transport(state):
    conn,result=connect(state,'guest.stream')
    if result!={'ready':True}:conn.close();raise ValueError('invalid stream acknowledgement')
    conn.settimeout(None)
    def upload():
        try:
            while chunk:=os.read(0,65536):conn.sendall(chunk)
            conn.shutdown(socket.SHUT_WR)
        except OSError:pass
    threading.Thread(target=upload,daemon=True).start()
    try:
        while chunk:=conn.recv(65536):
            view=memoryview(chunk)
            while view:
                count=os.write(1,view)
                if count<=0:raise OSError('stdout closed')
                view=view[count:]
    finally:conn.close()

def ssh_options(state):
    directory=state/'guest';config=json.loads((directory/'connection.json').read_text())
    if not re.fullmatch(r'[a-z_][a-z0-9_-]{0,31}',config['user']) or config['user']=='root':raise ValueError('invalid guest user')
    machine=state/'mac/machine.bin'
    if config['machine_sha256']!=hashlib.sha256(machine.read_bytes()).hexdigest():raise ValueError('VM identity changed; pair the new guest explicitly')
    command=shlex.join([sys.executable,str(ROOT/'scripts/guest-access.py'),'--state',str(state),'transport'])
    options=['-F','/dev/null','-i',str(directory/'client_key')]
    for value in ['BatchMode=yes','IdentitiesOnly=yes','IdentityAgent=none','ForwardAgent=no','ForwardX11=no',
                  'ClearAllForwardings=yes','PermitLocalCommand=no','StrictHostKeyChecking=yes','UpdateHostKeys=no',
                  'GlobalKnownHostsFile=/dev/null','UserKnownHostsFile='+json.dumps(str(directory/'known_hosts')),
                  'HostKeyAlias=warden-guest','HostKeyAlgorithms=ssh-ed25519','ProxyCommand='+command,
                  'ServerAliveInterval=30','ServerAliveCountMax=3','ConnectTimeout=15','EscapeChar=none',
                  'Port=2222','User='+config['user']]:options+=['-o',value]
    return options

def sftp_quote(value):
    if not value or any(c in value for c in '\n\r\0'):raise ValueError('file paths must be nonempty and single-line')
    if value.startswith('-'):value='./'+value
    # OpenSSH's batch parser protects glob characters inside double quotes.
    # Escaping them again produces literal backslashes in the resulting path.
    return '"'+''.join('\\'+c if c in '\\"' else c for c in value)+'"'

def record(state,event,operation,session,**fields):
    directory=state/'guest';directory.mkdir(exist_ok=True,mode=0o700)
    with (directory/'access.jsonl').open('a') as stream:
        os.fchmod(stream.fileno(),0o600);fcntl.flock(stream,fcntl.LOCK_EX)
        stream.write(json.dumps({'event':event,'operation':operation,'session':session,'time':time.time(),**fields})+'\n')
        stream.flush();os.fsync(stream.fileno())

def main(argv=None):
    os.umask(0o077)
    parser=argparse.ArgumentParser(description='Private guest management; SSH output is untrusted guest data')
    parser.add_argument('--state',type=Path,default=Path(os.environ.get('WARDEN_STATE',ROOT/'.local')))
    commands=parser.add_subparsers(dest='action',required=True)
    commands.add_parser('init',help='Generate the host-only client key')
    commands.add_parser('transport',help=argparse.SUPPRESS)
    pair=commands.add_parser('pair',help='Pin the fingerprint printed by the guest installer')
    pair.add_argument('--user',required=True);pair.add_argument('--fingerprint',required=True)
    pair.add_argument('--replace',action='store_true',help='Explicitly replace an existing pairing')
    commands.add_parser('shell');commands.add_parser('status')
    execute=commands.add_parser('exec');execute.add_argument('command',nargs=argparse.REMAINDER)
    for action in ('put','get'):
        transfer=commands.add_parser(action);transfer.add_argument('source');transfer.add_argument('destination')
    args=parser.parse_args(argv);state=args.state.resolve()
    if args.action=='transport':transport(state);return 0
    if args.action=='init':
        directory=initialize(state);print('Guest access identity ready:',directory/'client_key.pub');return 0
    if args.action=='pair':
        if not re.fullmatch(r'[a-z_][a-z0-9_-]{0,31}',args.user) or args.user=='root':raise ValueError('select an ordinary guest user')
        directory=initialize(state);conn,result=connect(state,'guest.keyscan');conn.close()
        fingerprint=key_fingerprint(result['key'])
        if fingerprint!=args.fingerprint:raise ValueError('guest fingerprint does not match; pairing refused')
        known='warden-guest '+result['key']+'\n';target=directory/'known_hosts'
        config={'user':args.user,'fingerprint':fingerprint,'machine_sha256':hashlib.sha256((state/'mac/machine.bin').read_bytes()).hexdigest()}
        previous=directory/'connection.json'
        if not args.replace and ((target.exists() and target.read_text()!=known) or (previous.exists() and json.loads(previous.read_text())!=config)):
            raise ValueError('pairing changed; inspect the guest and use --replace explicitly')
        target.write_text(known);previous.write_text(json.dumps(config,indent=2)+'\n')
        print('Pinned guest identity for',args.user, fingerprint);return 0
    options=ssh_options(state)
    if args.action in ('put','get'):
        source=args.source;destination=args.destination
        if args.action=='put':source=str(Path(source).resolve())
        else:destination=str(Path(destination).resolve())
        batch=args.action+' '+sftp_quote(source)+' '+sftp_quote(destination)+'\n'
        argv=['/usr/bin/sftp',*options,'-b','-','warden-guest'];input_data=batch.encode()
    else:
        input_data=None
        command=args.command if args.action=='exec' else (['/usr/bin/id'] if args.action=='status' else [])
        if command and command[0]=='--':command=command[1:]
        if args.action=='exec' and not command:raise ValueError('provide a guest command after exec --')
        argv=['/usr/bin/ssh',*options,'-tt' if args.action=='shell' else '-T','warden-guest']
        if command:argv.append(shlex.join(command))
    session=str(uuid.uuid4());record(state,'started',args.action,session)
    try:
        result=subprocess.run(argv,input=input_data)
        record(state,'completed',args.action,session,exit_code=result.returncode)
        return result.returncode
    except BaseException:
        record(state,'interrupted',args.action,session);raise
