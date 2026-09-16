"""Durable owner-approved Google document sharing. Called only by host services."""
import hashlib
import http.client
import json
import os
from pathlib import Path
import secrets
import sqlite3
import ssl
import threading
import time
from urllib.parse import urlencode, urlsplit, parse_qsl
from .google_docs import Connection, DOCUMENT_ID
from .core import Redactor

SCOPES = ('https://www.googleapis.com/auth/documents',
          'https://www.googleapis.com/auth/drive.metadata.readonly')

class GoogleConnection(Connection):
    def configure(self, client_id, client_secret, redirect_uri):
        # Public chat deployments terminate HTTPS at their authenticated edge.
        super().configure(client_id, client_secret, redirect_uri, allow_https=True)

    def __init__(self, root, config, clock=time.time):
        super().__init__(Redactor(), clock)
        self.root, self.config = Path(root), config
        self.granted_scopes = set()
        if config:
            info = Path(config).stat()
            if info.st_uid != os.getuid() or info.st_mode & 0o077:
                raise ValueError('Google configuration must be private')
            c = json.loads(Path(config).read_text())
            self.configure(c['client_id'], c['client_secret'], c['redirect_uri'])
        self.db = sqlite3.connect(self.root/'google.sqlite', check_same_thread=False)
        (self.root/'google.sqlite').chmod(0o600)
        self.db.execute('CREATE TABLE IF NOT EXISTS credentials (id INTEGER PRIMARY KEY, data TEXT)')
        row = self.db.execute('SELECT data FROM credentials WHERE id=1').fetchone()
        if row and self.client_id:
            data=json.loads(row[0])
            if data['client_id'] == self.client_id:
                self.access_token=data['access_token']; self.refresh_token=data['refresh_token']; self.expires=data['expires']
                self.granted_scopes=set(data.get('scopes',['https://www.googleapis.com/auth/documents.readonly','https://www.googleapis.com/auth/drive.metadata.readonly']))
                self.redactor.register(self.access_token); self.redactor.register(self.refresh_token)

    def can_write(self):
        return bool(self.access_token and 'https://www.googleapis.com/auth/documents' in self.granted_scopes)

    def create(self, title):
        if not self.can_write(): raise ValueError('Reconnect Google to allow document creation')
        conn=http.client.HTTPSConnection('docs.googleapis.com',timeout=15,context=ssl.create_default_context())
        try:
            conn.request('POST','/v1/documents',body=json.dumps({'title':title}),headers={'Authorization':self.authorization(),'Content-Type':'application/json'})
            res=conn.getresponse(); raw=res.read(1048577)
            if res.status!=200 or len(raw)>1048576: raise ValueError('Google document creation failed; check Google before retrying')
            data=json.loads(raw); id=data.get('documentId')
            if not isinstance(id,str) or not DOCUMENT_ID.fullmatch(id): raise ValueError('invalid created document response')
            return {'id':id,'title':data.get('title',title),'url':'https://docs.google.com/document/d/'+id+'/edit','api_url':'https://docs.googleapis.com/v1/documents/'+id}
        finally: conn.close()

    def start(self):
        url=super().start()
        parts=urlsplit(url); params=dict(parse_qsl(parts.query));params['scope']=' '.join(SCOPES)
        return parts._replace(query=urlencode(params)).geturl()

    def _install(self, data, initial=False):
        scopes=set(data.get('scope','').split()) if isinstance(data,dict) else set()
        if 'https://www.googleapis.com/auth/drive.metadata.readonly' not in scopes or not scopes.intersection({'https://www.googleapis.com/auth/documents','https://www.googleapis.com/auth/documents.readonly'}):
            raise ValueError('Google did not grant document and file-list access; reconnect')
        # Reuse the credential validation without changing the older adapter.
        super()._install({**data,'scope':'https://www.googleapis.com/auth/documents.readonly'},initial)
        self.granted_scopes=scopes
        with self.db:
            self.db.execute('INSERT OR REPLACE INTO credentials VALUES (1,?)',(json.dumps({'client_id':self.client_id,'access_token':self.access_token,'refresh_token':self.refresh_token,'expires':self.expires,'scopes':sorted(self.granted_scopes)}),))

    def files(self, page=''):
        params={'q':"trashed = false and mimeType = 'application/vnd.google-apps.document' and createdTime >= '2026-09-09T00:00:00Z'",'fields':'nextPageToken,files(id,name)','pageSize':100,'orderBy':'modifiedTime desc'}
        if page: params['pageToken']=page
        return self.get('/drive/v3/files?'+urlencode(params))

    def file(self, id):
        if not isinstance(id,str) or not DOCUMENT_ID.fullmatch(id):raise ValueError('invalid document')
        f=self.get('/drive/v3/files/'+id+'?fields=id,name,mimeType,trashed')
        if f.get('trashed') or f.get('mimeType')!='application/vnd.google-apps.document':raise ValueError('not an available Google document')
        return {'id':id,'title':f['name'],'url':'https://docs.google.com/document/d/'+id+'/edit','api_url':'https://docs.googleapis.com/v1/documents/'+id}

    def get(self, path):
        conn=http.client.HTTPSConnection('www.googleapis.com',timeout=8,context=ssl.create_default_context())
        try:
            conn.request('GET',path,headers={'Authorization':self.authorization(),'Accept':'application/json'})
            res=conn.getresponse();raw=res.read(1048577)
            if res.status!=200 or len(raw)>1048576:raise ValueError('Google file listing unavailable; reconnect or retry')
            return json.loads(raw)
        finally:conn.close()

class Sharing:
    def __init__(self, root, google=None, clock=time.time, github=None):
        self.clock=clock;self.google=google;self.github=github;self.lock=threading.RLock()
        root=Path(root);root.mkdir(mode=0o700,parents=True,exist_ok=True)
        self.db=sqlite3.connect(root/'sharing.sqlite',check_same_thread=False)
        (root/'sharing.sqlite').chmod(0o600)
        self.db.row_factory=sqlite3.Row
        self.db.execute('PRAGMA journal_mode=WAL')
        self.db.execute('PRAGMA synchronous=FULL')
        self.db.execute('CREATE TABLE IF NOT EXISTS requests (id TEXT PRIMARY KEY, chat TEXT, sandbox TEXT, reason TEXT, status TEXT, created REAL, expires REAL, documents TEXT, delivered INTEGER DEFAULT 0)')
        self.db.execute('CREATE TABLE IF NOT EXISTS repositories (chat TEXT, sandbox TEXT, owner TEXT, app INTEGER, name TEXT, id INTEGER, grant_id TEXT, PRIMARY KEY(chat,sandbox,name))')
        # Owner tags: a document tagged "unsharable with AI" can never be granted or fetched.
        self.db.execute("CREATE TABLE IF NOT EXISTS blocked_documents (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', blocked REAL)")
        columns={r[1] for r in self.db.execute('PRAGMA table_info(requests)')}
        for name, default in [('access','read'),('title','')]:
            if name not in columns: self.db.execute(f"ALTER TABLE requests ADD COLUMN {name} TEXT NOT NULL DEFAULT '{default}'")
        # A crash during a non-idempotent Google create is uncertain, never replay it.
        self.db.execute("UPDATE requests SET status='failed' WHERE status='creating'")
        self.db.commit()
        from .images import Images
        self.images = Images(self)
        from .pull_requests import PullRequests
        self.pull_requests = PullRequests(self)

    def blocked(self):
        return {r['id'] for r in self.db.execute('SELECT id FROM blocked_documents')}

    def result(self, row):
        return {'request_id':row['id'],'chatID':row['chat'],'sandboxID':row['sandbox'],'reason':row['reason'],'status':row['status'],'expires_at':row['expires'],'documents':json.loads(row['documents']),'access':row['access'],'title':row['title']}

    def dispatch(self, op, data):
        if op.startswith('image_'): return self.images.dispatch(op,data)
        if op=='pr_submit':
            try: return self.pull_requests.submit(data)
            except ValueError as exc: return {'status':'invalid','error':str(exc)}
        if op.startswith('pr_'): return self.pull_requests.dispatch(op, data)
        if op.startswith('github_'): return self.github_dispatch(op, data)
        with self.lock:
            if op=='status':return {'configured':bool(self.google and self.google.client_id),'connected':bool(self.google and self.google.access_token),'can_write':bool(self.google and self.google.can_write()),
                                    'github':{'connected':bool(self.github),'owner':self.github.owner if self.github else ''}}
            if op=='connect':return {'authorization_url':self.google.start()}
            if op=='callback':
                self.google.complete(data.get('state'),data.get('code'))
                # A reconnected Google account must never inherit old grants.
                with self.db:self.db.execute("UPDATE requests SET status='revoked' WHERE status='granted'")
                return {'ok':True}
            if op=='files':
                listing=self.google.files(data.get('page',''));blocked=self.blocked()
                return {**listing,'files':[{**f,'blocked':f.get('id') in blocked} for f in listing.get('files',[])]}
            if op=='blocked':
                return {'documents':[{'id':r['id'],'name':r['name'],'blocked_at':r['blocked']} for r in self.db.execute('SELECT * FROM blocked_documents ORDER BY blocked DESC, id')]}
            if op in ('block','unblock'):
                id=data.get('id');name=data.get('name','')
                if not isinstance(id,str) or not DOCUMENT_ID.fullmatch(id) or not isinstance(name,str) or len(name)>200:raise ValueError('invalid document')
                revoked=[]
                with self.db:
                    if op=='unblock':self.db.execute('DELETE FROM blocked_documents WHERE id=?',(id,))
                    else:
                        self.db.execute('INSERT OR REPLACE INTO blocked_documents VALUES (?,?,?)',(id,name,self.clock()))
                        # An existing grant must not outlive the tag.
                        for r in self.db.execute("SELECT id,documents FROM requests WHERE status='granted'"):
                            if any(d.get('id')==id for d in json.loads(r['documents'])):revoked.append(r['id'])
                        self.db.executemany("UPDATE requests SET status='revoked' WHERE id=?",[(r,) for r in revoked])
                return {'ok':True,'blocked':op=='block','revoked':revoked}
            if op=='select':
                requested=self.dispatch('request',{**data,'callID':'owner-'+secrets.token_hex(16),'reason':'Owner shared documents during conversation setup'})
                result=self.dispatch('resolve',{**data,'id':requested['request_id'],'allow':True})
                with self.db:self.db.execute('UPDATE requests SET delivered=1 WHERE id=?',(requested['request_id'],))
                return result
            if op=='request':
                chat,sandbox=data['chatID'],data['sandboxID'];reason=data.get('reason','')
                if not all(isinstance(v,str) and 0<len(v)<=128 for v in (chat,sandbox)) or not isinstance(reason,str) or not 1<=len(reason)<=2000:raise ValueError('invalid permission request')
                access=data.get('access','read');title=data.get('title','')
                if access not in ('read','write','create') or not isinstance(title,str) or len(title)>200 or (access=='create' and not title.strip()):raise ValueError('invalid document access or title')
                # Retry the same tool call without creating duplicate prompts.
                call=data.get('callID')
                if not isinstance(call,str) or not 1<=len(call)<=512:raise ValueError('invalid tool call')
                key=hashlib.sha256((chat+'\0'+sandbox+'\0'+call).encode()).hexdigest()
                with self.db:
                    self.db.execute('INSERT OR IGNORE INTO requests (id,chat,sandbox,reason,status,created,expires,documents,delivered,access,title) VALUES (?,?,?,?,?,?,?,?,0,?,?)',(key,chat,sandbox,reason,'pending',self.clock(),None,'[]',access,title))
                return self.result(self.db.execute('SELECT * FROM requests WHERE id=?',(key,)).fetchone())
            if op=='state':return {'requests':[self.result(r) for r in self.db.execute('SELECT * FROM requests ORDER BY created')]}
            # Grants belong to the environment (sandbox): every chat on it shares
            # one disk, so the requesting chat is attribution, not a boundary.
            if op=='get':
                r=self.db.execute('SELECT * FROM requests WHERE id=? AND sandbox=?',(data['id'],data['sandboxID'])).fetchone()
                if not r:raise ValueError('unknown request')
                return self.result(r)
            if op=='resolve':
                row=self.db.execute('SELECT * FROM requests WHERE id=?',(data['id'],)).fetchone()
                if not row:raise ValueError('unknown request')
                if row['status']!='pending':return self.result(row)
                docs=[];expiry=None;status='denied'
                if data.get('allow') is True:
                    if row['access'] in ('create','write') and not self.google.can_write():raise ValueError('Reconnect Google to allow writing documents')
                    ids=data.get('documents');ttl=data.get('duration')
                    if row['access']=='create':
                        if type(ttl)!=int or ttl not in (900,3600,86400,604800):raise ValueError('invalid duration')
                        with self.db:self.db.execute("UPDATE requests SET status='creating' WHERE id=?",(row['id'],))
                        try: docs=[self.google.create(row['title'])]
                        except Exception:
                            with self.db:self.db.execute("UPDATE requests SET status='failed' WHERE id=?",(row['id'],))
                            return self.result(self.db.execute('SELECT * FROM requests WHERE id=?',(row['id'],)).fetchone())
                        expiry=self.clock()+ttl;status='granted'
                    elif type(ttl)!=int or ttl not in (900,3600,86400,604800) or not isinstance(ids,list) or not 1<=len(ids)<=20 or len(set(ids))!=len(ids):raise ValueError('select 1–20 files and a valid duration')
                    elif self.blocked().intersection(ids):raise ValueError('a selected document is tagged unsharable with AI')
                    else: docs=[self.google.file(id) for id in ids];expiry=self.clock()+ttl;status='granted'
                with self.db:self.db.execute('UPDATE requests SET status=?,expires=?,documents=? WHERE id=?',(status,expiry,json.dumps(docs),data['id']))
                return self.result(self.db.execute('SELECT * FROM requests WHERE id=?',(data['id'],)).fetchone())
            if op=='revoke':
                with self.db:self.db.execute("UPDATE requests SET status='revoked' WHERE id=? AND status='granted'",(data['id'],))
                return {'ok':True}
            if op=='list':
                return {'grants':[self.result(r) for r in self.db.execute("SELECT * FROM requests WHERE sandbox=? AND status='granted' AND expires>?",(data['sandboxID'],self.clock()))]}
            if op=='undelivered':return {'requests':[self.result(r) for r in self.db.execute("SELECT * FROM requests WHERE status NOT IN ('pending','creating') AND delivered=0")]+[self.pull_requests.result(r) for r in self.db.execute("SELECT * FROM pull_requests WHERE status NOT IN ('pending','publishing') AND delivered=0")]}
            if op=='ack':
                with self.db:
                    self.db.execute('UPDATE requests SET delivered=1 WHERE id=?',(data['id'],))
                    self.db.execute('UPDATE pull_requests SET delivered=1 WHERE id=?',(data['id'],))
                return {'ok':True}
            raise ValueError('unknown sharing operation')

    def authorize(self, chat, sandbox, request):
        with self.lock:
            if request.get('scheme')!='https' or request.get('port')!=443 or request.get('host')!='docs.googleapis.com':raise ValueError('unsupported document authority')
            parsed=urlsplit(request.get('path',''))
            if parsed.scheme or parsed.netloc or parsed.fragment:raise ValueError('invalid document route')
            import base64
            from .document_writes import operation as shared_operation
            access,doc=shared_operation(request.get('method'),parsed.path,parse_qsl(parsed.query,keep_blank_values=True),base64.b64decode(request.get('body_base64',''),validate=True))
            rows=self.dispatch('list',{'chatID':chat,'sandboxID':sandbox})['grants']
            grant=next((r for r in rows if (access=='read' or r['access'] in ('write','create')) and any(d['id']==doc for d in r['documents'])),None)
            if not grant:raise ValueError('document is not shared with this environment')
            if doc in self.blocked():raise ValueError('document is tagged unsharable with AI')
            return grant,self.google.authorization()

    def active(self,id,chat,sandbox):
        with self.lock:
            r=self.db.execute("SELECT 1 FROM requests WHERE id=? AND sandbox=? AND status='granted' AND expires>?",(id,sandbox,self.clock())).fetchone()
            return bool(r)

    def github_dispatch(self, op, data):
        if not self.github: raise ValueError('GitHub App is not connected')
        owner, app = self.github.owner.lower(), self.github.app_id
        if op == 'github_repositories':
            return self.github.repositories(data.get('page', 1))
        chat, sandbox = data['chatID'], data['sandboxID']
        if not all(isinstance(v, str) and 0 < len(v) <= 128 for v in (chat, sandbox)):
            raise ValueError('invalid conversation')
        if op == 'github_select':
            names = data.get('repositories')
            if (not isinstance(names, list) or len(names) > 100 or any(not isinstance(n,str) for n in names)
                    or len(set(n.lower() for n in names)) != len(names)):
                raise ValueError('select up to 100 repositories')
            # Validate against fresh GitHub metadata before atomically replacing grants.
            found = {}; page = 1; wanted = {n.lower() for n in names}
            while wanted - found.keys():
                result = self.github.repositories(page)
                found.update({r['full_name'].lower(): r for r in result['repositories'] if r['full_name'].lower() in wanted})
                page = result['next_page']
                if not page: break
            if wanted != found.keys(): raise ValueError('repository is not owned by the connected account or available to Warden')
            with self.lock, self.db:
                self.db.execute('DELETE FROM repositories WHERE sandbox=?', (sandbox,))
                self.db.executemany('INSERT INTO repositories VALUES (?,?,?,?,?,?,?)',
                    [(chat,sandbox,owner,app,r['full_name'].lower(),r['id'],secrets.token_hex(32)) for r in found.values()])
        elif op != 'github_list': raise ValueError('unknown GitHub sharing operation')
        with self.lock:
            rows = self.db.execute('SELECT * FROM repositories WHERE sandbox=? AND owner=? AND app=? ORDER BY name', (sandbox,owner,app)).fetchall()
            return {'owner':owner,'repositories':[{'id':r['id'],'full_name':r['name'],
                'url':'https://github.com/'+r['name'], 'clone_url':'https://github.com/'+r['name']+'.git',
                'api_url':'https://api.github.com/repos/'+r['name'], 'access':'read', 'expires_at':None} for r in rows]}

    def github_grant(self, chat, sandbox, engine, request):
        if not self.github or not engine.network_enabled: return None
        if request.get('host') not in ('api.github.com','github.com') or request.get('scheme','https')!='https' or request.get('port',443)!=443:
            return None
        _, _, op, repo, _, _ = engine.normalize(request)
        from .github_app import permissions
        if not op or any(v != 'read' for v in permissions(op['operation_id']).values()): return None
        if request['host']=='api.github.com' and (request['method']!='GET' or request.get('body_base64')): return None
        with self.lock:
            row = self.db.execute('SELECT * FROM repositories WHERE sandbox=? AND name=? AND owner=? AND app=?',
                (sandbox,repo.lower(),self.github.owner.lower(),self.github.app_id)).fetchone()
            if not row: return None
            authorization = self.github.authorization(repo, op['operation_id'], repository_id=row['id'])
            return row['grant_id'], authorization

    def github_active(self, grant, chat, sandbox):
        if not self.github: return False
        with self.lock:
            return bool(self.db.execute('SELECT 1 FROM repositories WHERE grant_id=? AND sandbox=? AND owner=? AND app=?',
                (grant,sandbox,self.github.owner.lower(),self.github.app_id)).fetchone())
