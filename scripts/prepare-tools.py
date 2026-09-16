#!/usr/bin/env python3
"""Fetch pinned official-vendor installers, without running or signing in."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess

ROOT=Path(__file__).resolve().parents[1]; STATE=Path(os.environ.get('WARDEN_STATE',ROOT/'.local')).resolve()
def run(*args): subprocess.run([str(a) for a in args],check=True)
def digest(path):
    value=hashlib.sha256()
    with path.open('rb') as stream:
        while data:=stream.read(1024*1024): value.update(data)
    return value.hexdigest()
def main():
    dest=STATE/'guest-assets'; dest.mkdir(parents=True,exist_ok=True)
    lock=json.loads((ROOT/'config/tools.lock.json').read_text())
    for name,artifact in lock['artifacts'].items():
        path=dest/name
        if not path.exists() and os.environ.get('WARDEN_OFFLINE')=='1':raise SystemExit('Offline mode: missing '+name)
        if not path.exists(): run('curl','-fsSL','--retry','3',artifact['url'],'-o',path)
        if digest(path)!=artifact['sha256']: raise SystemExit('checksum mismatch: '+name)
    if os.environ.get('WARDEN_OFFLINE')=='1':
        offline=json.loads((ROOT/'config/offline-inputs.json').read_text())['files']
        name='guest-assets/agent-tools.tar.gz'
        if digest(STATE/name)!=offline[name]['sha256']:raise SystemExit('Offline agent tooling checksum mismatch')
        names=[*lock['artifacts'],'agent-tools.tar.gz']
        (dest/'SHA256SUMS').write_text(''.join(digest(dest/name)+'  '+name+'\n' for name in names))
        (dest/'DEVTOOLS-SHA256SUMS').write_text(''.join(digest(dest/name)+'  '+name+'\n' for name in ['gh.zip','Homebrew.pkg']))
        print('Verified local guest tooling; no network used.'); return
    tooling=STATE/'guest-tooling'; tooling.mkdir(exist_ok=True)
    for name in ['package.json','package-lock.json']: shutil.copy2(ROOT/'config/tooling'/name,tooling/name)
    run('npm','ci','--prefix',tooling,'--ignore-scripts')
    run('tar','-czf',dest/'agent-tools.tar.gz','-C',tooling,'node_modules')
    names=[*lock['artifacts'],'agent-tools.tar.gz']
    (dest/'SHA256SUMS').write_text(''.join(digest(dest/name)+'  '+name+'\n' for name in names))
    (dest/'DEVTOOLS-SHA256SUMS').write_text(''.join(digest(dest/name)+'  '+name+'\n' for name in ['gh.zip','Homebrew.pkg']))
    print('Unsigned-in guest tooling prepared:',dest)
if __name__=='__main__': main()
