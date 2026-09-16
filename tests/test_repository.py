"""Real Git clone/fetch/push through Guard + Engine + local git-http-backend.

No external repository, credential, or approval is used. Only DNS and the
upstream network are replaced; protocol parsing, repacking, review, grants and
the stock Git client's HTTP exchanges execute for real.
"""
import asyncio
import base64
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import AsyncMock, patch
from urllib.parse import urlsplit

from warden.core import Engine
from warden.git_protocol import route, push, ZERO
from warden.server import internal

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT/'proxy'))
from git_review import inspect_objects, run_git
try:
    from mitmproxy import http, connection
    HAVE_MITM=True
except ImportError: HAVE_MITM=False

def pkt(data): return f'{len(data)+4:04x}'.encode()+data
def payload(old='1'*40,new='2'*40,ref='refs/heads/work',caps=b'report-status side-band-64k agent=git/2.43.0'):
    return pkt(f'{old} {new} {ref}'.encode()+b'\0'+caps)+b'0000'

class GitProtocolTests(unittest.TestCase):
    def test_only_canonical_services(self):
        for method,path in [('GET','/acme/demo.git/info/refs?service=git-upload-pack'),('POST','/acme/demo.git/git-upload-pack'),('POST','/acme/demo.git/git-receive-pack')]: self.assertIsNotNone(route(method,path))
        for path in ['/acme/demo/info/refs?service=git-upload-pack','/acme/demo.git/info/refs?service=git-upload-pack&x=1','/acme/../demo.git/git-upload-pack','/acme/demo.git/HEAD','/acme/demo.git/git-upload-pack?x=1','/acme/demo.git/objects/info/packs']:
            self.assertIsNone(route('GET',path))
    def test_push_restrictions(self):
        update,_=push(payload());self.assertEqual(update['ref'],'refs/heads/work')
        for body in [payload(new=ZERO),payload(ref='refs/tags/v1'),payload(ref='refs/heads/../x'),payload(ref='refs/heads/x.lock'),payload(caps=b'push-options'),payload(caps=b'object-format=sha256'),payload()+b'junk',b'0001',b'fffftiny',payload().replace(b'0000',pkt(b'extra')+b'0000')]:
            with self.subTest(body=body),self.assertRaises(ValueError): push(body)

@unittest.skipUnless(HAVE_MITM,'mitmproxy required for repository integration')
class RepositoryIntegrationTests(unittest.TestCase):
    def git(self,cwd,*args,check=True):
        env={**os.environ,'GIT_CONFIG_NOSYSTEM':'1','GIT_CONFIG_GLOBAL':'/dev/null','GIT_TERMINAL_PROMPT':'0',
             'GIT_AUTHOR_NAME':'Warden Test','GIT_AUTHOR_EMAIL':'test@example.invalid',
             'GIT_COMMITTER_NAME':'Warden Test','GIT_COMMITTER_EMAIL':'test@example.invalid'}
        result=subprocess.run(['git','-c','credential.helper=','-C',str(cwd),*args],capture_output=True,env=env,timeout=30)
        if check and result.returncode: self.fail(result.stderr.decode())
        return result
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name)
        self.remote=self.root/'acme/demo.git';self.remote.parent.mkdir()
        self.seed=self.root/'seed';self.seed.mkdir()
        self.git(self.seed,'init','-b','main')
        (self.seed/'hello.txt').write_text('hello\n')
        self.git(self.seed,'add','.');self.git(self.seed,'commit','-m','Initial')
        self.git(self.root,'clone','--bare',str(self.seed),str(self.remote));self.git(self.remote,'config','http.receivepack','true')
        self.engine=Engine(self.root/'state');self.token='ghp_'+'TESTONLY'*8;self.engine.set_token(self.token)
        spec=importlib.util.spec_from_file_location('repository_addon',ROOT/'proxy/addon.py')
        self.addon=importlib.util.module_from_spec(spec);spec.loader.exec_module(self.addon)
        self.guard=self.addon.Guard();self.upstream=[]
        test=self
        async def dispatch(message): return internal(test.engine,message)
        async def review(repository,body,auth,upstream,active):
            self.assertEqual(repository,'acme/demo')
            self.assertEqual(auth,'Basic '+base64.b64encode(('x-access-token:'+self.token).encode()).decode())
            with tempfile.TemporaryDirectory() as directory:
                async def git(*args,data=None,limit=262144): return await run_git(directory,list(args),data=data,active=active,limit=limit)
                await git('init','--bare','--quiet','--template=')
                await git('-c','protocol.file.allow=always','fetch','--quiet',str(self.remote),'+refs/heads/*:refs/heads/*')
                if await git('for-each-ref','--format=%(refname)','refs/heads/'):
                    await git('-c','protocol.file.allow=always','fetch','--quiet',str(self.remote),'+HEAD:refs/warden/base')
                update,pack=push(body)
                return await inspect_objects(git,update,pack,body)
        self.callpatch=patch.object(self.addon,'call',dispatch);self.callpatch.start()
        self.reviewpatch=patch.object(self.addon,'inspect_push',review);self.reviewpatch.start()
        class Handler(BaseHTTPRequestHandler):
            def log_message(self,*args): pass
            def do_GET(self): self.handle_git()
            def do_POST(self): self.handle_git()
            def handle_git(self):
                data=self.rfile.read(int(self.headers.get('Content-Length','0')))
                async def handle():
                    client=connection.Client(peername=('10.77.0.2',1234),sockname=('140.82.116.5',443),sni='github.com')
                    flow=http.HTTPFlow(client,connection.Server(address=('140.82.116.5',443)),live=True)
                    headers=dict(self.headers);headers['Host']='github.com';headers['Authorization']='Bearer guest-credential'
                    flow.request=http.Request.make(self.command,'https://github.com'+self.path,data,headers)
                    answers=[(socket.AF_INET,socket.SOCK_STREAM,6,'',('140.82.116.5',443))]
                    with patch.object(asyncio.get_running_loop(),'getaddrinfo',AsyncMock(return_value=answers)):
                        await test.guard.requestheaders(flow);await test.guard.request(flow)
                    if flow.response is None:
                        test.upstream.append((flow.request.path,flow.request.headers.get('Authorization'),flow.request.content))
                        parts=urlsplit(self.path)
                        env={**os.environ,'GIT_PROJECT_ROOT':str(test.root),'GIT_HTTP_EXPORT_ALL':'1',
                             'PATH_INFO':parts.path,'REQUEST_METHOD':self.command,'QUERY_STRING':parts.query,
                             'CONTENT_TYPE':flow.request.headers.get('Content-Type',''),'CONTENT_LENGTH':str(len(flow.request.content)),
                             'REMOTE_USER':'warden-test','SERVER_PROTOCOL':'HTTP/1.1','GIT_CONFIG_NOSYSTEM':'1',
                             'GIT_CONFIG_GLOBAL':'/dev/null','GIT_PROTOCOL':flow.request.headers.get('Git-Protocol','')}
                        result=subprocess.run(['git','http-backend'],input=flow.request.content,capture_output=True,env=env,timeout=20)
                        header,body=result.stdout.split(b'\r\n\r\n',1)
                        values=dict(line.decode().split(': ',1) for line in header.split(b'\r\n'))
                        status=int(values.pop('Status','200 OK').split()[0])
                        flow.response=http.Response.make(status,body,values)
                    await test.guard.response(flow)
                    return flow.response
                response=asyncio.run(handle())
                self.send_response(response.status_code)
                self.send_header('Content-Type',response.headers.get('Content-Type','application/octet-stream'))
                self.send_header('Content-Length',str(len(response.content)));self.end_headers();self.wfile.write(response.content)
        self.server=ThreadingHTTPServer(('127.0.0.1',0),Handler)
        self.thread=threading.Thread(target=self.server.serve_forever,daemon=True);self.thread.start()
        self.url=f'http://127.0.0.1:{self.server.server_address[1]}/acme/demo.git'
        self.work=self.root/'work'
    def tearDown(self):
        self.server.shutdown();self.server.server_close();self.callpatch.stop();self.reviewpatch.stop()
        self.engine.db.close();os.close(self.engine.audit.fd);self.tmp.cleanup()
    def pending(self,operation):
        row=self.engine.db.execute("SELECT * FROM requests WHERE operation=? AND status='pending' ORDER BY created DESC",(operation,)).fetchone()
        self.assertIsNotNone(row,operation+' missing from approval queue');return row
    def checkout(self):
        denied=self.git(self.root,'clone',self.url,str(self.work),check=False)
        self.assertNotEqual(denied.returncode,0);self.assertIn(b'428',denied.stderr)
        self.assertFalse(self.upstream)
        row=self.pending('git/read');grant=self.engine.approve(row['id'],'scoped',600)
        self.git(self.root,'clone',self.url,str(self.work))
        self.assertEqual((self.work/'hello.txt').read_text(),'hello\n')
        self.git(self.work,'switch','-c','feature')
        return grant
    def change(self,text='verified-source-private-marker\n'):
        (self.work/'hello.txt').write_text(text);self.git(self.work,'add','.');self.git(self.work,'commit','-m','Test change')
    def push(self,check=False):return self.git(self.work,'-c','http.postBuffer=8388608','push','--no-thin','origin','HEAD:refs/heads/feature',check=check)
    def test_complete_clone_review_push_fetch_and_credential_retention(self):
        grant=self.checkout();self.change();result=self.push()
        self.assertIn(b'428',result.stderr)
        row=self.pending('git/push');review=self.engine.git_review(row['id'])
        self.assertIn('verified-source-private-marker',review['patch']);self.assertEqual(review['update']['ref'],'refs/heads/feature')
        with self.assertRaises(ValueError): self.engine.approve(row['id'],'scoped',60)
        for file in (self.root/'state').rglob('*'):
            if file.is_file():
                self.assertNotIn(b'verified-source-private-marker',file.read_bytes());self.assertNotIn(self.token.encode(),file.read_bytes())
        self.engine.approve(row['id'],'exact',60)
        self.push(check=True)
        self.assertEqual(self.git(self.remote,'rev-parse','refs/heads/feature').stdout,self.git(self.work,'rev-parse','HEAD').stdout)
        self.git(self.work,'fetch','origin')
        self.assertTrue(all(auth.startswith('Basic ') and 'guest-credential' not in auth for _,auth,_ in self.upstream))
        self.engine.revoke(grant['grant_id'])
        self.assertNotEqual(self.git(self.work,'fetch','origin',check=False).returncode,0)
    def test_approval_cannot_publish_a_changed_commit(self):
        self.checkout();self.change();self.push();row=self.pending('git/push');self.engine.approve(row['id'],'exact',60)
        self.change('second change\n');result=self.push();self.assertIn(b'428',result.stderr)
        self.assertNotEqual(self.git(self.remote,'rev-parse','--verify','refs/heads/feature',check=False).returncode,0)
    def test_binary_hidden_in_history_is_blocked(self):
        self.checkout()
        (self.work/'binary').write_bytes(b'\0secret');self.git(self.work,'add','.');self.git(self.work,'commit','-m','Binary')
        self.git(self.work,'rm','binary');self.git(self.work,'commit','-m','Remove binary')
        result=self.push();self.assertIn(b'403',result.stderr)
        self.assertIsNone(self.engine.db.execute("SELECT id FROM requests WHERE operation='git/push'").fetchone())
    def test_force_push_rejected_even_with_read_permission(self):
        self.checkout();self.change();self.push();self.engine.approve(self.pending('git/push')['id'],'exact',60);self.push(check=True)
        self.git(self.work,'reset','--hard','main');self.change('divergent\n')
        result=self.git(self.work,'push','--force','--no-thin','origin','HEAD:refs/heads/feature',check=False)
        self.assertIn(b'403',result.stderr)
    def test_first_push_to_empty_repository(self):
        self.git(self.remote,'update-ref','-d','refs/heads/main')
        result=self.git(self.root,'clone',self.url,str(self.work),check=False);self.assertIn(b'428',result.stderr)
        self.engine.approve(self.pending('git/read')['id'],'scoped',600)
        self.git(self.root,'clone',self.url,str(self.work));self.git(self.work,'switch','-c','feature')
        self.change();result=self.push();self.assertIn(b'428',result.stderr)
        row=self.pending('git/push');self.assertIn('verified-source-private-marker',self.engine.git_review(row['id'])['patch'])
        self.engine.approve(row['id'],'exact',60);self.push(check=True)
    def test_repository_scope_and_review_expiry(self):
        self.checkout()
        value=self.engine.authorize({'method':'GET','host':'github.com','path':'/acme/other.git/info/refs?service=git-upload-pack','headers':[]})
        self.assertEqual(value['status'],428);self.assertNotIn('authorization',value)
        self.change();self.push();row=self.pending('git/push')
        expiry,review=self.engine.git_reviews[row['fingerprint']]
        self.engine.git_reviews[row['fingerprint']]=(0,review)
        with self.assertRaises(ValueError): self.engine.approve(row['id'],'exact',60)
        with self.assertRaises(ValueError): self.engine.git_review(row['id'])
    def test_repack_discards_unreachable_guest_objects(self):
        self.checkout();self.change()
        # Construct a valid pack containing an extra private blob that is not in
        # the branch. The stock client's normal pack would not include it.
        secret_path=self.work/'untracked-secret';secret_path.write_text('unreachable-secret-marker')
        hidden=self.git(self.work,'hash-object','-w',str(secret_path)).stdout.strip()
        head=self.git(self.work,'rev-parse','HEAD').stdout.strip()
        objects=self.git(self.work,'rev-list','--objects','HEAD','--not','--remotes').stdout.splitlines()
        ids=b'\n'.join(line.split(b' ')[0] for line in objects)+b'\n'+hidden+b'\n'
        pack=subprocess.run(['git','-C',str(self.work),'pack-objects','--stdout'],input=ids,capture_output=True,check=True).stdout
        body=payload(old=ZERO,new=head.decode(),ref='refs/heads/feature')+pack
        async def verify():
            with tempfile.TemporaryDirectory() as directory:
                async def git(*args,data=None,limit=262144):return await run_git(directory,list(args),data=data,limit=limit)
                await git('init','--bare','--quiet','--template=')
                await git('-c','protocol.file.allow=always','fetch','--quiet',str(self.remote),'+refs/heads/*:refs/heads/*','+HEAD:refs/warden/base')
                review,clean=await inspect_objects(git,push(body)[0],pack,body)
                with tempfile.TemporaryDirectory() as output:
                    await run_git(output,['init','--bare','--quiet','--template='])
                    _,cleanpack=push(clean)
                    await run_git(output,['index-pack','--stdin'],data=cleanpack)
                    with self.assertRaises(ValueError): await run_git(output,['cat-file','-t',hidden.decode()])
                self.assertNotIn('unreachable-secret-marker',review['patch'])
        asyncio.run(verify())

if __name__=='__main__': unittest.main()
