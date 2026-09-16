"""Private GitHub App installation credential broker; no user PAT or OAuth paste.

The broker runs alongside Panta's existing encrypted GitHub store. Its stdin/stdout
protocol is for trusted host processes only, never an agent or public HTTP route.
"""
from __future__ import annotations
import argparse
import base64
from datetime import datetime
import http.client
import json
import os
from pathlib import Path
import re
import sqlite3
import ssl
import stat
import subprocess
import threading
import time

REPOSITORY = re.compile(r'[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}')
# Start with explicit app-supported repository operations. Unknown operations
# cannot silently inherit all installation permissions.
CONTENTS_READ = {'git/read','repos/get-content','repos/list-branches','repos/get-branch',
                 'repos/list-commits','repos/get-commit','repos/compare-commits',
                 'git/get-blob','git/get-tree','git/get-commit','git/get-ref','git/list-matching-refs'}
CONTENTS_WRITE = {'git/push','git/create-blob','git/create-tree','git/create-commit',
                  'git/create-ref','git/update-ref','git/delete-ref','repos/create-or-update-file-contents',
                  'repos/delete-file','repos/merge'}
PULL_READ = {'pulls/get','pulls/list','pulls/list-files','pulls/list-commits','pulls/list-reviews','pulls/list-review-comments'}
PULL_WRITE = {'pulls/create','pulls/update','pulls/merge','pulls/create-review','pulls/create-review-comment'}

def permissions(operation):
    if operation == 'repos/get': return {'metadata':'read'}
    if operation in CONTENTS_READ: return {'contents':'read','metadata':'read'}
    if operation in CONTENTS_WRITE: return {'contents':'write','metadata':'read'}
    if operation in PULL_READ: return {'pull_requests':'read','metadata':'read'}
    if operation in PULL_WRITE: return {'pull_requests':'write','metadata':'read'}
    raise ValueError('operation is not supported by the GitHub App broker')


def private_read(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_size > limit:
            raise ValueError('credential configuration must be a private regular file')
        with os.fdopen(fd,'rb',closefd=False) as f: return f.read(limit+1)
    finally: os.close(fd)


def request(method, path, token, body=None):
    conn=http.client.HTTPSConnection('api.github.com',timeout=15,context=ssl.create_default_context())
    try:
        conn.request(method,path,body=json.dumps(body) if body is not None else None,
                     headers={'Authorization':'Bearer '+token,'Accept':'application/vnd.github+json',
                              'X-GitHub-Api-Version':'2022-11-28','User-Agent':'Warden-GitHub-App',
                              'Content-Type':'application/json'})
        response=conn.getresponse(); data=response.read(1048577)
        if len(data)>1048576:
            raise ValueError('GitHub response exceeds the 1 MiB inspection limit')
        if response.status not in (200,201):
            raise ValueError('GitHub App request failed; check installation access')
        return json.loads(data)
    finally: conn.close()


class StoreBroker:
    def __init__(self, directory, app_id, owner, transport=request, clock=time.time):
        self.directory=Path(directory); self.app_id=app_id; self.owner=owner
        self.transport,self.clock=transport,clock

    def app(self):
        # Reuse only the app record. Never read/decrypt Panta user tokens.
        from cryptography.hazmat.primitives.ciphers.aead import AESGCM
        key=private_read(self.directory/'encryption.key',32)
        db=sqlite3.connect('file:'+str(self.directory/'github.sqlite')+'?mode=ro',uri=True)
        try: row=db.execute('SELECT value FROM secrets WHERE key=?',('app',)).fetchone()
        finally: db.close()
        if not row: raise ValueError('existing GitHub App record is missing')
        sealed=row[0]
        app=json.loads(AESGCM(key).decrypt(sealed[:12],sealed[12:],b'app'))
        if app.get('id')!=self.app_id or app.get('slug')!='monaddle-workspace':
            raise ValueError('unexpected GitHub App identity')
        return app

    def jwt(self, app):
        from cryptography.hazmat.primitives import serialization, hashes
        from cryptography.hazmat.primitives.asymmetric import padding, rsa
        def enc(b): return base64.urlsafe_b64encode(b).rstrip(b'=')
        now=int(self.clock())
        data=enc(b'{"alg":"RS256","typ":"JWT"}')+b'.'+enc(json.dumps({
            'iat':now-60,'exp':now+540,'iss':str(self.app_id)},separators=(',',':')).encode())
        key=serialization.load_pem_private_key(app['pem'].encode(),password=None)
        if not isinstance(key,rsa.RSAPrivateKey) or key.key_size<2048:raise ValueError('invalid app signing key')
        return (data+b'.'+enc(key.sign(data,padding.PKCS1v15(),hashes.SHA256()))).decode()

    def repositories(self, page=1):
        if type(page) is not int or not 1 <= page <= 10000:
            raise ValueError('invalid repository page')
        jwt = self.jwt(self.app())
        installation = self.transport('GET', '/users/'+self.owner+'/installation', jwt)
        account = installation.get('account', {})
        if (installation.get('app_id') != self.app_id or installation.get('suspended_at') is not None
                or account.get('login', '').lower() != self.owner.lower() or account.get('type') != 'User'
                or type(installation.get('id')) is not int or installation['id'] <= 0):
            raise ValueError('unexpected GitHub account installation')
        credential = self.transport('POST', '/app/installations/'+str(installation['id'])+'/access_tokens', jwt,
                                    {'permissions': {'metadata': 'read'}})
        if credential.get('permissions') != {'metadata': 'read'}:
            raise ValueError('unexpected discovery permissions')
        data = self.transport('GET', '/installation/repositories?per_page=100&page='+str(page), credential['token'])
        repos = []
        for repo in data['repositories']:
            name = repo.get('full_name', '')
            if (REPOSITORY.fullmatch(name) and name.split('/')[0].lower() == self.owner.lower()
                    and repo.get('owner', {}).get('login', '').lower() == self.owner.lower()):
                repos.append({'id': repo['id'], 'full_name': name, 'private': bool(repo.get('private'))})
        return {'owner': self.owner, 'app_id': self.app_id, 'repositories': repos,
                'next_page': page+1 if len(data['repositories']) == 100 else None,
                'installation_id': installation['id']}

    def issue(self, value):
        if not isinstance(value,dict) or set(value)!={'repository','operation'}:raise ValueError('invalid broker request')
        repo=value['repository']
        if not isinstance(repo,str) or not REPOSITORY.fullmatch(repo) or repo.split('/')[0].lower()!=self.owner.lower():
            raise ValueError('repository outside configured installation owner')
        perms=permissions(value['operation'])
        jwt=self.jwt(self.app())
        installation=self.transport('GET','/repos/'+repo+'/installation',jwt)
        if (installation.get('app_id')!=self.app_id or installation.get('suspended_at') is not None
                or installation.get('account',{}).get('login','').lower()!=self.owner.lower()
                or type(installation.get('id')) is not int or installation['id']<=0):
            raise ValueError('unexpected or suspended installation')
        result=self.transport('POST','/app/installations/'+str(installation['id'])+'/access_tokens',jwt,
                              {'repositories':[repo.split('/')[1]],'permissions':perms})
        returned=result.get('permissions',{})
        if returned!=perms:raise ValueError('unexpected installation token permissions')
        repositories=result.get('repositories')
        if (not isinstance(repositories,list) or len(repositories)!=1
                or repositories[0].get('full_name','').lower()!=repo.lower()):
            raise ValueError('unexpected installation token repositories')
        return {'token':result['token'],'expires_at':result['expires_at'],'repository':repo,
                'permissions':perms,'app_id':self.app_id,'installation_id':installation['id'], 'repository_id':repositories[0].get('id')}


class Credentials:
    """One trusted-host source can serve multiple Engines; grants remain separate."""
    def __init__(self, command, owner, app_id, redactor, clock=time.time):
        self.command,self.owner,self.app_id=command,owner,app_id
        self.redactor,self.clock=redactor,clock
        self.lock=threading.RLock()

    def authorization(self, repository, operation, repository_id=None):
        if (not isinstance(repository,str) or not REPOSITORY.fullmatch(repository)
                or repository.split('/')[0].lower()!=self.owner.lower()):
            raise ValueError('repository outside configured GitHub App owner')
        perms=permissions(operation)
        with self.lock:
            # Mint only after a grant matched. Do not cache between approvals: new
            # grants recheck current installation access and repository selection.
            result=subprocess.run(self.command,input=json.dumps({'repository':repository,'operation':operation}).encode(),
                                  stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=45,check=True)
            if len(result.stdout)>65536:raise ValueError('oversized broker response')
            data=json.loads(result.stdout)
            token=data.get('token')
            if (not isinstance(token,str) or not re.fullmatch(r'ghs_[A-Za-z0-9._~-]{16,8192}',token)
                    or data.get('repository','').lower()!=repository.lower() or data.get('permissions')!=perms
                    or data.get('app_id')!=self.app_id):raise ValueError('invalid broker response')
            if repository_id is not None and data.get('repository_id') != repository_id:
                raise ValueError('repository identity changed; select it again')
            expires=datetime.fromisoformat(data['expires_at'].replace('Z','+00:00')).timestamp()
            if not self.clock()+30<expires<=self.clock()+3700:raise ValueError('invalid installation expiry')
            self.redactor.register(token)
            return 'Bearer '+token

    def repositories(self, page=1):
        if type(page) is not int or not 1 <= page <= 10000:
            raise ValueError('invalid repository page')
        result = subprocess.run(self.command, input=json.dumps({'action':'repositories','page':page}).encode(),
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=45, check=True)
        if len(result.stdout) > 65536: raise ValueError('oversized repository listing')
        data = json.loads(result.stdout)
        if data.get('owner', '').lower() != self.owner.lower() or data.get('app_id') != self.app_id:
            raise ValueError('unexpected repository account')
        for repo in data['repositories']:
            if (not REPOSITORY.fullmatch(repo['full_name']) or repo['full_name'].split('/')[0].lower() != self.owner.lower()
                    or type(repo['id']) is not int or repo['id'] <= 0):
                raise ValueError('invalid repository listing')
        return data

    def snapshot(self):
        return {'configured':True,'app_id':self.app_id,'owner':self.owner,'identity':'installation',
                'credential_source':'private_host_broker','manual_token_required':False}


def from_config(path, redactor, clock=time.time):
    data=json.loads(private_read(path,16384))
    if (set(data)!={'command','owner','app_id'} or not isinstance(data['command'],list) or not data['command']
            or any(not isinstance(v,str) or not v or '\x00' in v for v in data['command'])
            or not re.fullmatch(r'[A-Za-z0-9-]{1,100}',data['owner'])
            or type(data['app_id']) is not int or data['app_id']<=0):raise ValueError('invalid broker configuration')
    return Credentials(data['command'],data['owner'],data['app_id'],redactor,clock)


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--store',required=True);parser.add_argument('--app-id',required=True,type=int)
    parser.add_argument('--owner',required=True)
    args=parser.parse_args()
    import sys
    try:
        data=sys.stdin.buffer.read(4097)
        if len(data)>4096:raise ValueError('oversized input')
        value=json.loads(data)
        broker=StoreBroker(args.store,args.app_id,args.owner)
        result=broker.repositories(value['page']) if isinstance(value,dict) and set(value)=={'action','page'} and value['action']=='repositories' else broker.issue(value)
        sys.stdout.write(json.dumps(result))
    except Exception:
        sys.stdout.write('{"error":"GitHub App credential unavailable"}')
        sys.exit(1)

if __name__=='__main__':main()
