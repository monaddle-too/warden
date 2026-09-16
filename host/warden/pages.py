"""Bounded, read-only dashboard pages. Never materialize an entire audit log."""
import base64
import hashlib
import json
import math
import os
import threading
from urllib.parse import parse_qsl

PAGE_SIZE = 25
SCAN_BYTES = 512 * 1024
RECORD_BYTES = 64 * 1024
SUMMARY_BYTES = 16 * 1024
READERS = threading.BoundedSemaphore(2)
TRAFFIC_TYPES = frozenset(('http.request', 'http.request.external', 'http.response',
    'request.allowed', 'request.denied', 'request.interrupted', 'proxy.error',
    'dns.query', 'network.denied', 'tls.passthrough','egress.allowed','egress.denied','network.connected','network.disconnected'))
STATUSES = frozenset(('all','pending','approved','executed','denied','stale'))

class StaleCursor(ValueError): pass


def parameters(query, allowed):
    pairs = parse_qsl(query, keep_blank_values=True, max_num_fields=6)
    if len(dict(pairs)) != len(pairs) or set(dict(pairs))-allowed:
        raise ValueError('invalid page parameters')
    result = dict(pairs)
    if len(result.get('q','')) > 128 or len(result.get('cursor','')) > 1024:
        raise ValueError('page parameter too long')
    return result


def pack(value):
    return base64.urlsafe_b64encode(json.dumps(value,separators=(',',':')).encode()).decode().rstrip('=')


def unpack(value):
    try: return json.loads(base64.b64decode(value+'='*(-len(value)%4),altchars=b'-_',validate=True))
    except Exception: raise ValueError('invalid page cursor') from None


def clip(value, limit=1024):
    return value[:limit] if isinstance(value,str) else ''


def request_summary(summary, redactor):
    if not isinstance(summary,dict): return {}
    out = {key:clip(summary[key],2048 if key=='path' else 256)
        for key in ('method','host','path','operation','repository','http_version') if isinstance(summary.get(key),str)}
    if isinstance(summary.get('update'),dict):
        out['update']={key:clip(summary['update'][key],256) for key in ('ref','old','new') if isinstance(summary['update'].get(key),str)}
    if 'path' in out:
        out['path'] = out['path'].split('?')[0]
        if '?' in summary['path']: out['query'] = '[REDACTED]'
    headers = summary.get('headers',[])
    pairs = [[clip(p[0],64),clip(p[1],256)] for p in headers[:16]
        if isinstance(p,(list,tuple)) and len(p)==2] if isinstance(headers,list) else []
    if pairs: out['headers'] = redactor.headers(pairs)
    if isinstance(summary.get('body'),dict) and type(summary['body'].get('bytes')) is int:
        out['body_bytes'] = summary['body']['bytes']
    return redactor.clean(out)


def history_page(engine, params):
    status=params.get('status','all'); query=params.get('q','').strip().lower()
    if status not in STATUSES: raise ValueError('invalid history status')
    clauses=[]; args=[]
    if status!='all': clauses.append('status=?'); args.append(status)
    if query:
        clauses.append('(instr(lower(id),?) OR instr(lower(operation),?) OR instr(lower(path),?) OR instr(lower(repository),?))')
        args.extend([query]*4)
    if params.get('cursor'):
        cursor=unpack(params['cursor'])
        if not isinstance(cursor,list) or len(cursor)!=4 or cursor[2:]!=[status,query] or type(cursor[0]) not in (int,float) or not math.isfinite(cursor[0]) or not isinstance(cursor[1],str) or len(cursor[1])>36:
            raise ValueError('invalid history cursor')
        clauses.append('(created,id)<(?,?)');args.extend(cursor[:2])
    where=' WHERE '+' AND '.join(clauses) if clauses else ''
    rows=[]
    # SQLite applies filtering and keyset paging on disk. Cap each projected
    # summary before it enters Python, including any pre-retention legacy rows.
    with engine.lock:
        cursor=engine.db.execute('SELECT id,status,created,substr(summary,1,?),operation,substr(path,1,2048),substr(repository,1,256) FROM requests'+where+' ORDER BY created DESC,id DESC LIMIT ?', [SUMMARY_BYTES]+args+[PAGE_SIZE+1])
        for row in cursor:
            if len(rows)==PAGE_SIZE: break
            try: summary=json.loads(row[3])
            except (ValueError,TypeError): summary={'operation':row[4],'path':row[5],'repository':row[6]}
            rows.append({'id':row[0],'status':row[1],'created':row[2], 'summary':request_summary(summary,engine.redactor)})
        else: row=None
    more=row is not None and len(rows)==PAGE_SIZE
    next_cursor=pack([rows[-1]['created'],rows[-1]['id'],status,query]) if more else None
    return {'items':rows,'next_cursor':next_cursor,'page_size':PAGE_SIZE}


def traffic_record(event, redactor):
    if not isinstance(event,dict) or event.get('event_type') not in TRAFFIC_TYPES: return None
    out={key:clip(event.get(key),2048 if key=='reason' else 256) for key in
        ('event_id','event_type','time','request_id','hostname','reason') if isinstance(event.get(key),str)}
    if type(event.get('status')) is int: out['status']=event['status']
    request=request_summary(event.get('request'),redactor)
    if request: out['request']=request
    response=event.get('response')
    if isinstance(response,dict): out['response']=request_summary(response,redactor)
    destination=event.get('destination')
    if isinstance(destination,dict): out['destination']={clip(k,32):v if type(v) is int else clip(v,256) for k,v in list(destination.items())[:12]}
    return redactor.clean(out)


def traffic_page(engine, params):
    kind=params.get('type','all'); query=params.get('q','').strip().lower()
    if kind!='all' and kind not in TRAFFIC_TYPES: raise ValueError('invalid traffic type')
    if not READERS.acquire(blocking=False): raise BlockingIOError('traffic reader busy')
    try:
        with engine.audit.path.open('rb',buffering=0) as stream:
            stat=os.fstat(stream.fileno())
            identity=[stat.st_dev,stat.st_ino]
            snapshot=end=stat.st_size
            prior=None
            if params.get('cursor'):
                prior=unpack(params['cursor'])
                if not isinstance(prior,list) or len(prior)!=7 or prior[5:]!=[kind,query] or prior[0]!=identity or prior[4]!=1:
                    raise StaleCursor('Traffic log changed; refresh to latest.')
                snapshot,end=prior[1:3]
                if type(snapshot) is not int or type(end) is not int or not 0<=end<=snapshot<=stat.st_size:
                    raise StaleCursor('Traffic log changed; refresh to latest.')
            # Detect replacement/truncation, including a file that regrew. Normal
            # appends do not change the fixed end-of-snapshot anchor.
            stream.seek(max(0,snapshot-128)); anchor=hashlib.sha256(stream.read(min(128,snapshot))).hexdigest()
            if prior and prior[3]!=anchor: raise StaleCursor('Traffic log changed; refresh to latest.')
            start=max(0,end-SCAN_BYTES)
            stream.seek(max(0,start-1)); previous=stream.read(1) if start else b'\n'
            stream.seek(start); data=stream.read(end-start)
            # Do not parse a partial first/last record, even when an oversized
            # record spans several pages. Every read remains <= SCAN_BYTES.
            lower=0
            if previous!=b'\n':
                first=data.find(b'\n'); lower=first+1 if first>=0 else len(data)
            upper=len(data)
            if data and not data.endswith(b'\n'): upper=data.rfind(b'\n')+1
            rows=[]; skipped=0; boundary=start
            while upper>lower:
                line_end=upper-1
                line_start=data.rfind(b'\n',lower,line_end)+1
                if line_start<lower: line_start=lower
                size=line_end-line_start
                boundary=start+line_start
                if 0<size<=RECORD_BYTES:
                    try: item=traffic_record(json.loads(data[line_start:line_end]),engine.redactor)
                    except (ValueError,TypeError,AttributeError,RecursionError): item=None;skipped+=1
                    if item and (kind=='all' or item['event_type']==kind) and (not query or query in json.dumps(item,ensure_ascii=False).lower()):
                        rows.append(item)
                        if len(rows)==PAGE_SIZE: break
                elif size>RECORD_BYTES: skipped+=1
                upper=line_start
            else:
                # Re-read the partial prefix next time, or skip a bounded portion
                # of an oversized record. This never skips a complete small row.
                boundary=start+lower if 0<lower<len(data) else start
            more=boundary>0
            cursor=pack([identity,snapshot,boundary,anchor,1,kind,query]) if more else None
            return {'items':rows,'next_cursor':cursor,'page_size':PAGE_SIZE,
                    'scanned_bytes':len(data),'skipped_records':skipped,'snapshot_bytes':snapshot}
    finally: READERS.release()
