#!/usr/bin/env python3
"""Read the native audit and emit OCSF, JSONL, ECS, or RFC 5424 syslog."""
import argparse
import json
from pathlib import Path
import socket
import sys
import time
sys.path.insert(0,str(Path(__file__).resolve().parents[1]/'host'))
from warden.ocsf import Exporter
parser=argparse.ArgumentParser(); parser.add_argument('path',type=Path); parser.add_argument('--format',choices=['ocsf','jsonl','ecs','syslog'],default='jsonl'); parser.add_argument('--follow',action='store_true')
args=parser.parse_args()
ocsf=Exporter()
severity={'debug':7,'info':6,'warning':4,'error':3,'critical':2}
with args.path.open() as stream:
    while True:
        position=stream.tell(); line=stream.readline()
        if not line or not line.endswith('\n'):
            if not args.follow: break
            stream.seek(position); time.sleep(.25); continue
        event=json.loads(line)
        if args.format=='jsonl': output=event
        elif args.format=='ocsf': output=ocsf.convert(event)
        elif args.format=='ecs':
            action=event['event_type']
            output={'@timestamp':event['time'],'event':{'id':event['event_id'],'action':action,'kind':'event','category':['network'] if action.startswith(('http.','network.','dns.')) else ['iam'],'outcome':'failure' if action.endswith(('.denied','.error','.interrupted')) else 'unknown'},'observer':{'product':'Warden','version':event['producer']['version']},'log':{'level':event['severity']},'warden':event}
            if event.get('request_id'): output['trace']={'id':event['request_id']}
        else:
            # local0 facility; JSON payload is the RFC 5424 MSG. No outbound
            # network connection or transmission occurs in this exporter.
            print(f"<{16*8+severity[event['severity']]}>1 {event['time']} {socket.gethostname().split('.')[0]} warden - {event['event_type']} - {json.dumps(event,separators=(',',':'))}",flush=True); continue
        print(json.dumps(output,separators=(',',':'),ensure_ascii=args.format!='ocsf',allow_nan=False),flush=True)
