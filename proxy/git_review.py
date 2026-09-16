"""Produce a push preview in the trusted Linux VM, never on the physical host.

No checkout, hooks, filters, submodules, local config, or guest executable runs.
Scratch objects live on tmpfs and are removed after review. Git itself remains a
trusted parser: resource limits reduce denial of service, not parser exploits.
"""
import asyncio
import base64
import hashlib
import json
import os
from pathlib import Path
import signal
import sys
import tempfile
import time

from warden.git_protocol import ZERO, push

LIMIT = 262144
SCRATCH_LIMIT = 256 * 1024 * 1024

async def run_git(directory, args, *, data=None, auth=None, upstream=None, active=None, limit=LIMIT):
    env = {'PATH':'/usr/bin:/bin', 'HOME':str(directory), 'LANG':'C', 'LC_ALL':'C',
           'GIT_CONFIG_NOSYSTEM':'1', 'GIT_CONFIG_GLOBAL':'/dev/null',
           'GIT_TERMINAL_PROMPT':'0', 'GIT_NO_REPLACE_OBJECTS':'1',
           'GIT_CONFIG_COUNT':'1', 'GIT_CONFIG_KEY_0':'credential.helper', 'GIT_CONFIG_VALUE_0':'',
           'PYTHONPATH':str(Path(__file__).resolve().parents[1]/'host')}
    if auth:
        env.update(GIT_CONFIG_COUNT='3', GIT_CONFIG_KEY_1='http.extraHeader', GIT_CONFIG_VALUE_1='Authorization: '+auth,
                   GIT_CONFIG_KEY_2='http.curloptResolve', GIT_CONFIG_VALUE_2='github.com:443:'+upstream)
    argv = ['git', '-c','core.hooksPath=/dev/null', '-c','protocol.allow=never', '-c','protocol.https.allow=always',
            '-c','http.followRedirects=false', '-c','http.sslVerify=true', '-c','http.proxy=',
            '-c','core.attributesFile=/dev/null', '-c','core.pager=cat', '-C',str(directory), *args]
    if sys.platform == 'linux':
        # Set limits in a fresh child, not preexec_fn in a threaded service.
        argv = [sys.executable, str(Path(__file__).resolve()), '--limited', *argv]
    process = await asyncio.create_subprocess_exec(*argv, env=env, stdin=asyncio.subprocess.PIPE,
        stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL, start_new_session=True)
    async def io():
        async def send():
            try:
                if data: process.stdin.write(data); await process.stdin.drain()
            except (BrokenPipeError, ConnectionResetError): pass
            finally: process.stdin.close()
        writer = asyncio.create_task(send())
        result = bytearray()
        try:
            while chunk := await process.stdout.read(65536):
                result.extend(chunk)
                if len(result)>limit: raise ValueError('review or pack exceeds size limit; split the change')
            await writer
            if await process.wait() != 0: raise ValueError('Git review failed; refresh the remote, use a non-thin push, and retry')
            return bytes(result)
        finally:
            writer.cancel()
    task = asyncio.create_task(io())
    started = time.monotonic()
    try:
        while not task.done():
            await asyncio.wait({task}, timeout=.25)
            if time.monotonic()-started > 90: raise ValueError('Git review timed out')
            if active and not await active(): raise ValueError('repository read permission expired during review')
            if sum(p.stat().st_size for p in Path(directory).rglob('*') if p.is_file()) > SCRATCH_LIMIT:
                raise ValueError('repository exceeds review scratch limit')
        return await task
    finally:
        if process.returncode is None:
            try: os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError: pass
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)
        await process.wait()

async def inspect_push(repository, body, auth, upstream, active):
    update, pack = push(body)
    scratch = '/dev/shm' if sys.platform == 'linux' else None
    with tempfile.TemporaryDirectory(prefix='warden-review-', dir=scratch) as directory:
        async def git(*args, data=None, network=False, limit=LIMIT):
            return await run_git(directory, list(args), data=data, auth=auth if network else None,
                                 upstream=upstream, active=active, limit=limit)
        await git('init','--bare','--quiet','--template=')
        # Reuse exactly the approved repository identity, with independent DNS
        # pinning, upstream TLS verification and no redirect/credential helpers.
        await git('fetch','--quiet','--no-tags','--no-recurse-submodules',
                  'https://github.com/'+repository+'.git',
                  '+refs/heads/*:refs/heads/*','+refs/tags/*:refs/tags/*', network=True)
        if await git('for-each-ref','--format=%(refname)','refs/heads/'):
            await git('fetch','--quiet','--no-tags','--no-recurse-submodules',
                      'https://github.com/'+repository+'.git','+HEAD:refs/warden/base',network=True)
        return await inspect_objects(git, update, pack, body)

async def inspect_objects(git, update, pack, body):
    """Also used by integration tests against genuine temporary Git repositories."""
    if pack: await git('index-pack','--stdin','--fix-thin','--strict',data=pack)
    new = update['new']; old = update['old']
    if (await git('cat-file','-t',new)).strip() != b'commit': raise ValueError('branch target must be a commit')
    if old != ZERO:
        current = (await git('rev-parse','--verify',update['ref'])).strip().decode()
        if current != old: raise ValueError('remote branch changed; fetch and rebase before pushing')
        await git('merge-base','--is-ancestor',old,new)  # Force pushes fail closed.
        base = old
    else:
        refs = (await git('for-each-ref','--format=%(refname)')).splitlines()
        if update['ref'].encode() in refs: raise ValueError('branch already exists; fetch before pushing')
        if b'refs/warden/base' in refs:
            base = (await git('rev-parse','--verify','refs/warden/base')).strip().decode()
            # A new feature branch is reviewed against its actual fork point.
            base = (await git('merge-base',base,new)).strip().decode()
        else:
            base = (await git('hash-object','-w','-t','tree','--stdin',data=b'')).strip().decode()
    # Inspect ALL objects/commits being sent, including merged histories that a
    # tip-only diff would hide. Existing remote history is already upstream.
    outgoing = [new,'--not','--all']
    # Connectivity and SHA verification are done by Git, not a guest report.
    await git('rev-list','--objects','--missing=error',new,'--not','--all')
    raw = await git('log','--format=','--raw','--no-renames','--diff-merges=first-parent',*outgoing,'--')
    if b'160000' in raw: raise ValueError('submodule updates require a separate workflow and are blocked')
    numstat = await git('log','--format=','--numstat','--no-renames','--diff-merges=first-parent',*outgoing,'--')
    if any(line.startswith(b'-\t-\t') for line in numstat.splitlines()):
        raise ValueError('binary changes cannot be reviewed here; publish text-only changes')
    patch = await git('log','--format=fuller','--patch','--diff-merges=first-parent',
                      '--no-ext-diff','--no-textconv','--no-renames','--no-color',*outgoing,'--')
    stat = await git('diff','--stat','--no-renames',base,new,'--')
    commits = await git('log','--no-show-signature','--format=%H %s',*outgoing)
    # Repack only objects reachable from the approved tip and missing upstream.
    # Guest-supplied unreachable objects never hitch a ride to GitHub. Use fixed
    # packing options so an identical retry produces identical permission bytes.
    known = (await git('for-each-ref','--format=%(objectname)')).splitlines()
    revisions = new.encode()+b'\n'+b''.join(b'^'+oid+b'\n' for oid in known)
    clean = await git('-c','pack.threads=1','pack-objects','--stdout','--revs',data=revisions,limit=8*1024*1024)
    from warden.git_protocol import packet
    _, offset = packet(body,0)
    _, offset = packet(body,offset)
    body = body[:offset]+clean
    def visible(data):
        text=data.decode('utf-8',errors='backslashreplace')
        return ''.join('\\u%04x'%ord(c) if (ord(c)<32 and c not in '\n\t') or ord(c) in (127,0x202a,0x202b,0x202c,0x202d,0x202e,0x2066,0x2067,0x2068,0x2069) else c for c in text)
    return {'update':update, 'base':base, 'pack_request_sha256':hashlib.sha256(body).hexdigest(),
            'patch':visible(patch), 'stat':visible(stat), 'commits':visible(commits), 'truncated':False}, body

if __name__ == '__main__' and sys.argv[1:2] == ['--limited']:
    import resource
    resource.setrlimit(resource.RLIMIT_FSIZE,(128*1024*1024,128*1024*1024))
    resource.setrlimit(resource.RLIMIT_AS,(768*1024*1024,768*1024*1024))
    resource.setrlimit(resource.RLIMIT_CPU,(60,60))
    os.execvpe(sys.argv[2], sys.argv[2:], os.environ)
