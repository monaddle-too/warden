"""Forward Linux DNS and rejected-packet metadata to the host audit writer."""
import json
from pathlib import Path
import re
import subprocess
import threading
import time
from addon import rpc

def deliver(event_type,fields):
    while True:
        try:
            rpc({'action':'event','event_type':event_type,'fields':fields}); return
        except Exception: time.sleep(1)

def dns():
    path=Path('/var/log/warden-dns.log')
    while not path.exists(): time.sleep(1)
    with path.open() as stream:
        stream.seek(0,2)
        while True:
            line=stream.readline()
            if not line: time.sleep(.2); continue
            match=re.search(r'query\[([^]]+)\] (\S+) from (\S+)',line)
            if match: deliver('dns.query',{'hostname':match[2],'reason':'DNS '+match[1], 'destination':{'resolver':'10.77.0.1','client':match[3]}})

threading.Thread(target=dns,daemon=True).start()
process=subprocess.Popen(['journalctl','-k','-f','-o','json','--since=now'],stdout=subprocess.PIPE,text=True)
for line in process.stdout:
    try:
        message=json.loads(line).get('MESSAGE','')
        if not message.startswith('WARDEN_DENY '): continue
        values=dict(re.findall(r'(SRC|DST|SPT|DPT|PROTO|IN|OUT)=([^ ]+)',message))
        deliver('network.denied',{'reason':'Linux firewall dropped a packet','destination':values})
    except (ValueError,TypeError): continue
