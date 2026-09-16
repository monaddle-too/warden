#!/usr/bin/env python3
import json
import os
from pathlib import Path
import socket
import sys
state=Path(os.environ.get('WARDEN_STATE',Path(__file__).resolve().parents[1]/'.local'))
with socket.socket(socket.AF_UNIX,socket.SOCK_STREAM) as conn:
    conn.settimeout(195); conn.connect(str(state/'vm-management.sock'))
    conn.sendall((json.dumps({'argv':sys.argv[1:],'timeout':180})+'\n').encode())
    result=json.loads(conn.makefile('rb').readline(3*1024*1024))
    print(result.get('stdout',''),end=''); print(result.get('stderr',''),end='',file=sys.stderr)
    if 'error' in result: print(result['error'],file=sys.stderr)
    sys.exit(result.get('exit_code',1))
