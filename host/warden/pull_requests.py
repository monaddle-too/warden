"""Immutable, owner-reviewed pull request proposals. No guest write credentials."""
import base64
import difflib
import hashlib
import json
import re
import threading
from urllib.parse import quote
from .github_app import permissions, request

LIMIT = 256 * 1024

def text(value, limit, empty=True):
    if not isinstance(value, str) or len(value.encode('utf-8')) > limit or (not empty and not value.strip()):
        raise ValueError('invalid or oversized proposal text')
    if any(ord(c) < 32 and c not in '\n\r\t' for c in value) or any(c in value for c in '\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069'):
        raise ValueError('binary or hidden control characters are not supported')
    return value

def sha(value):
    if not isinstance(value,str) or not re.fullmatch('[0-9a-f]{40}',value): raise ValueError('invalid Git object')
    return value

def reviewed_body(proposal, outgoing_body):
    """Fail closed if a publisher attempts to send a body other than the owner's approval."""
    expected = proposal.get('approved_body_sha256')
    if (not isinstance(outgoing_body,str) or outgoing_body != proposal['body']
            or hashlib.sha256(outgoing_body.encode('utf-8')).hexdigest() != expected):
        raise ValueError('Pull request body differs from the approved review. Submit it for review again.')


def relevant_entries(call, root, paths):
    """Read only affected directories, pinned to the base tree's immutable SHAs."""
    entries = {}; loaded = set()
    def directory(prefix, tree):
        if prefix in loaded: return
        listing = call('GET', '/git/trees/' + sha(tree), 'git/get-tree')
        if listing.get('truncated'): raise ValueError('Repository directory is too large to review safely')
        for item in listing['tree']:
            name = item['path']
            if not isinstance(name, str) or not name or '/' in name or name in ('.', '..'):
                raise ValueError('Invalid repository tree entry')
            path = prefix + name
            if path in entries: raise ValueError('Duplicate repository tree entry')
            entries[path] = item
        loaded.add(prefix)
    directory('', root)
    for path in sorted(paths):
        parts = path.split('/')
        for i in range(1, len(parts)):
            parent = '/'.join(parts[:i]); entry = entries.get(parent)
            if not entry or entry['type'] != 'tree': break
            directory(parent + '/', entry['sha'])
    return entries


class PullRequests:
    def __init__(self, sharing, transport=request):
        self.s = sharing; self.transport = transport
        with self.s.lock, self.s.db:
            self.s.db.execute('CREATE TABLE IF NOT EXISTS pull_requests (id TEXT PRIMARY KEY, chat TEXT, sandbox TEXT, status TEXT, proposal TEXT, outcome TEXT, delivered INTEGER DEFAULT 0)')
            self.s.db.execute("UPDATE pull_requests SET status='failed', outcome=? WHERE status='publishing'",(json.dumps({'error':'Publication interrupted. Check the proposal branch on GitHub before submitting again; Warden will not retry automatically.'}),))

    def row(self, id):
        return self.s.db.execute('SELECT * FROM pull_requests WHERE id=?',(id,)).fetchone()

    def result(self, row, preview=False):
        p=json.loads(row['proposal'])
        out={'request_id':row['id'],'kind':'pull_request','chatID':row['chat'],'sandboxID':row['sandbox'],
             'status':row['status'],'repository':p['repository'],'title':p['title'],'head':p['head'],**json.loads(row['outcome'])}
        if preview: out['proposal']=p
        return out

    def active(self, data, repo, repository_id=None):
        repos=self.s.github_dispatch('github_list',data)['repositories']
        r=next((r for r in repos if r['full_name'].lower()==repo.lower()),None)
        if not r or (repository_id is not None and r['id']!=repository_id):
            raise ValueError('Select this repository for the conversation before proposing a pull request')
        return r['id']

    def api(self, repo, repository_id):
        credentials=self.s.github; tokens={}
        def call(method, suffix, operation, body=None):
            key=json.dumps(permissions(operation),sort_keys=True)
            if key not in tokens:
                tokens[key]=credentials.authorization(repo,operation,repository_id=repository_id).removeprefix('Bearer ')
            return self.transport(method,'/repos/'+repo+suffix,tokens[key],body)
        return call

    def submit(self, data):
        required={'chatID','sandboxID','callID','repository','base','title','body','files'}
        if set(data)-{'images'}!=required: raise ValueError('Provide repository, base branch, title, body, and changed files')
        repo=text(data['repository'],201,False).lower()
        rid=self.active(data,repo)
        for key in ('chatID','sandboxID','callID'): text(data[key],512,False)
        id=hashlib.sha256(('pr\0'+data['chatID']+'\0'+data['sandboxID']+'\0'+data['callID']).encode()).hexdigest()
        with self.s.lock:
            old=self.row(id)
            if old: return self.result(old)
        title=text(data['title'],256,False);body=text(data['body'],32768)
        branch=text(data['base'],200,False)
        if not re.fullmatch(r'[A-Za-z0-9_./-]+',branch) or '..' in branch or any(x in ('','.','..') or x.endswith('.lock') for x in branch.split('/')):
            raise ValueError('invalid base branch')
        files=data['files']
        if not isinstance(files,list) or len(files)>20 or (not files and not data.get('images')): raise ValueError('Submit up to 20 text files and/or image attachments')
        paths=set();size=0
        for f in files:
            if not isinstance(f,dict) or set(f)!={'path','content'}: raise ValueError('Each file requires path and complete content (null to delete)')
            path=text(f['path'],512,False)
            if any(c in path for c in '\\\n\r\t') or any(p in ('','.','..') or p.lower()=='.git' for p in path.split('/')):
                raise ValueError('invalid repository path')
            if path.startswith('.github/workflows/'): raise ValueError('Workflow changes require separate GitHub App permissions and are not supported here')
            if path in paths: raise ValueError('duplicate path')
            paths.add(path)
            if f['content'] is not None: size+=len(text(f['content'],LIMIT).encode())
        if size>LIMIT: raise ValueError('Proposal exceeds 256 KiB; split it into smaller pull requests')
        for path in paths:
            if any(path.startswith(other+'/') for other in paths if other!=path): raise ValueError('overlapping file paths')
        call=self.api(repo,rid)
        base=sha(call('GET','/git/ref/heads/'+quote(branch,safe='/'),'git/get-ref')['object']['sha'])
        tree=sha(call('GET','/git/commits/'+base,'git/get-commit')['tree']['sha'])
        lookup_paths = paths | ({'.warden/images/attachment.png'} if data.get('images') else set())
        entries=relevant_entries(call,tree,lookup_paths);changes=[];total=size
        for f in files:
            path=f['path'];entry=entries.get(path);before=None;mode='100644'
            for ancestor in ('/'.join(path.split('/')[:i]) for i in range(1,len(path.split('/')))):
                if ancestor in entries and entries[ancestor]['type']!='tree': raise ValueError('path crosses a non-directory')
            if entry:
                if entry['type']!='blob' or entry['mode'] not in ('100644','100755'): raise ValueError('Only regular text files can be reviewed')
                if entry.get('size',0)>LIMIT: raise ValueError('Base file exceeds review limit')
                blob=call('GET','/git/blobs/'+sha(entry['sha']),'git/get-blob')
                if blob.get('encoding')!='base64': raise ValueError('unsupported file encoding')
                before=text(base64.b64decode(blob['content']).decode('utf-8'),LIMIT);mode=entry['mode']
                total+=len(before.encode())
            after=f['content']
            if before==after: raise ValueError('Every submitted file must contain a change')
            if total>LIMIT*2: raise ValueError('Combined before/after text exceeds review limit')
            diff=list(difflib.unified_diff((before or '').splitlines(keepends=True),(after or '').splitlines(keepends=True),fromfile='a/'+path if before is not None else '/dev/null',tofile='b/'+path if after is not None else '/dev/null'))
            # Preserve missing-newline information instead of visually joining +/- lines.
            lines=[]
            for line in diff:
                lines.append(line.rstrip('\n'))
                if not line.endswith('\n'): lines.append('\\ No newline at end of file')
            changes.append({'path':path,'content':after,'mode':mode,'change':'added' if before is None else 'deleted' if after is None else 'modified',
                'diff':lines,'additions':sum(l.startswith('+') for l in diff[2:]),'deletions':sum(l.startswith('-') for l in diff[2:])})
        image_ids=data.get('images',[])
        if not isinstance(image_ids,list) or len(image_ids)>4 or len(set(image_ids))!=len(image_ids):raise ValueError('Select up to four distinct images')
        images=[]
        for image_id in image_ids:
            image=self.s.images.get(image_id,data['chatID'],data['sandboxID'])
            path='.warden/images/'+image['digest']+'.png'
            if path in paths or path in entries or any(path.startswith(other+'/') for other in paths):raise ValueError('Image path conflicts with proposed or existing files')
            for ancestor in ('.warden','.warden/images'):
                if ancestor in entries and entries[ancestor]['type']!='tree':raise ValueError('Image directory is not a regular directory')
            url='../blob/warden/pr-'+id[:24]+'/'+path+'?raw=true'
            images.append({'image_id':image_id,'sha256':image['digest'],'path':path,'url':url,'caption':image['caption']})
            body+='\n\n![Screenshot '+str(len(images))+']('+url+')\n'
        text(body,32768)
        proposal={'images':images,'repository':repo,'repository_id':rid,'owner':self.s.github.owner,'app_id':self.s.github.app_id,'title':title,'body':body,'base':branch,'base_sha':base,'base_tree':tree,'head':'warden/pr-'+id[:24],'files':changes}
        if len(json.dumps(proposal).encode())>1024*1024: raise ValueError('Rendered proposal exceeds review limit')
        with self.s.lock,self.s.db:
            self.active(data,repo,rid)
            self.s.db.execute('INSERT OR IGNORE INTO pull_requests VALUES (?,?,?,?,?,?,0)',(id,data['chatID'],data['sandboxID'],'pending',json.dumps(proposal),'{}'))
            return self.result(self.row(id))

    def dispatch(self, op, data):
        if op=='pr_submit': return self.submit(data)
        with self.s.lock:
            if op=='pr_state': return {'requests':[self.result(r) for r in self.s.db.execute('SELECT * FROM pull_requests ORDER BY rowid')]}
            row=self.row(data.get('id'))
            if not row: raise ValueError('unknown pull request proposal')
            if op=='pr_preview': return self.result(row,True)
            if op=='pr_get':
                if row['chat']!=data.get('chatID') or row['sandbox']!=data.get('sandboxID'): raise ValueError('proposal belongs to another conversation')
                return self.result(row)
            if op!='pr_resolve' or type(data.get('allow')) is not bool: raise ValueError('invalid review decision')
            if row['status']!='pending':
                if data['allow'] and row['status'] in ('publishing','published') and 'body' in data:
                    reviewed_body(json.loads(row['proposal']),data['body'])
                return self.result(row)
            feedback=text(data.get('feedback',''),4000)
            p=json.loads(row['proposal'])
            if data['allow'] and 'body' not in data: raise ValueError('Approval must include the exact reviewed PR body')
            body=text(data.get('body',p['body']),32768)
            p['submitted_body']=p['body']
            p['body']=body
            outcome={'feedback':feedback}
            if data['allow']: p['approved_body_sha256']=hashlib.sha256(body.encode('utf-8')).hexdigest()
            elif body!=p['submitted_body']: outcome['edited_body']=body
            status='publishing' if data['allow'] else 'rejected'
            with self.s.db:self.s.db.execute('UPDATE pull_requests SET status=?,proposal=?,outcome=? WHERE id=?',(status,json.dumps(p),json.dumps(outcome),row['id']))
            result=self.result(self.row(row['id']))
            if data['allow']: threading.Thread(target=self.publish,args=(row['id'],),daemon=True).start()
            return result

    def publish(self, id):
        # No retry after an ambiguous response: creating a PR is not an idempotent API.
        with self.s.lock:
            row=self.row(id)
            if not row or row['status']!='publishing': return
            p=json.loads(row['proposal']);data={'chatID':row['chat'],'sandboxID':row['sandbox']}
        phase='validating';out={}
        try:
            reviewed_body(p,p['body'])
            if self.s.github.owner!=p['owner'] or self.s.github.app_id!=p['app_id']: raise ValueError('Connected GitHub account changed; submit a new proposal')
            self.active(data,p['repository'],p['repository_id'])
            call=self.api(p['repository'],p['repository_id'])
            current=call('GET','/git/ref/heads/'+quote(p['base'],safe='/'),'git/get-ref')['object']['sha']
            if current!=p['base_sha']: raise ValueError('The base branch changed since review. Submit a fresh proposal for a new review.')
            image_entries=[]
            for image in p.get('images',[]):
                stored=self.s.images.get(image['image_id'],row['chat'],row['sandbox'])
                if stored['digest']!=image['sha256']:raise ValueError('Reviewed image changed')
                blob=call('POST','/git/blobs','git/create-blob',{'content':base64.b64encode(stored['png']).decode(),'encoding':'base64'})
                image_entries.append({'path':image['path'],'mode':'100644','type':'blob','sha':sha(blob['sha'])})
            phase='creating tree'
            tree=call('POST','/git/trees','git/create-tree',{'base_tree':p['base_tree'],'tree':[{'path':f['path'],'mode':f['mode'],'type':'blob',**({'sha':None} if f['content'] is None else {'content':f['content']})} for f in p['files']]+image_entries})
            phase='creating commit'
            commit=call('POST','/git/commits','git/create-commit',{'message':p['title'],'tree':sha(tree['sha']),'parents':[p['base_sha']]})
            self.active(data,p['repository'],p['repository_id'])
            # Check again before making a branch externally visible.
            if call('GET','/git/ref/heads/'+quote(p['base'],safe='/'),'git/get-ref')['object']['sha']!=p['base_sha']: raise ValueError('The base branch changed; submit a new proposal')
            phase='creating branch'
            call('POST','/git/refs','git/create-ref',{'ref':'refs/heads/'+p['head'],'sha':sha(commit['sha'])})
            out['branch_url']='https://github.com/'+p['repository']+'/tree/'+p['head']
            phase='creating pull request'
            self.active(data,p['repository'],p['repository_id'])
            payload={'title':p['title'],'body':p['body'],'base':p['base'],'head':p['head'],'maintainer_can_modify':False}
            reviewed_body(p,payload['body'])
            pr=call('POST','/pulls','pulls/create',payload)
            if type(pr.get('number')) is not int or pr['number']<=0: raise ValueError('GitHub returned an unexpected pull request response')
            out.update({'url':'https://github.com/'+p['repository']+'/pull/'+str(pr['number']),'number':pr['number']});status='published'
        except Exception as exc:
            # Do not return upstream response bodies or credential diagnostics to the agent.
            out.update({'error':str(exc) if isinstance(exc,ValueError) else 'GitHub publication failed. Check installation permissions and GitHub before submitting again.',
                        'phase':phase,'branch_url':'https://github.com/'+p['repository']+'/tree/'+p['head']});status='failed'
        with self.s.lock,self.s.db:
            self.s.db.execute('UPDATE pull_requests SET status=?,outcome=? WHERE id=?',(status,json.dumps(out),id))
