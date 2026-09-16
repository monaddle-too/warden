#!/usr/bin/env python3
"""Build a fresh Linux appliance from a checksum-pinned Canonical image."""
import hashlib
import base64
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import urllib.request

ROOT=Path(__file__).resolve().parents[1]
STATE=Path(os.environ.get('WARDEN_STATE',ROOT/'.local')).resolve()
def run(*args): subprocess.run([str(a) for a in args],check=True)
def fetch(url,path,sha=None):
    if not path.exists():
        if os.environ.get('WARDEN_OFFLINE')=='1':raise SystemExit('Offline mode: missing '+path.name)
        tmp=path.with_suffix(path.suffix+'.part'); run('curl','-fsSL','--retry','3',url,'-o',tmp); tmp.replace(path)
    if sha:
        h=hashlib.sha256()
        with path.open('rb') as stream:
            while chunk:=stream.read(1024*1024): h.update(chunk)
        if h.hexdigest()!=sha: raise SystemExit('checksum mismatch: '+str(path))

def sync():
    sys.path.insert(0,str(ROOT/'host'))
    from warden.guest import initialize
    access=initialize(STATE)
    payload=STATE/'assets/payload'; payload.mkdir(parents=True,exist_ok=True)
    for folder in ['proxy','host','vendor','tests','config']:
        shutil.copytree(ROOT/folder,payload/folder,dirs_exist_ok=True,ignore=shutil.ignore_patterns('__pycache__','*.pyc'))
    (payload/'guest-assets').mkdir(exist_ok=True)
    shutil.copy2(ROOT/'scripts/guest-bootstrap.sh',payload/'guest-assets/bootstrap.sh')
    for name in ['install-guest-clipboard.sh','install-guest-dev-tools.sh','install-guest-access.sh','warden-repo']:
        shutil.copy2(ROOT/'scripts'/name, payload/'guest-assets'/name)
    if (STATE/'guest-assets').exists(): shutil.copytree(STATE/'guest-assets',payload/'guest-assets',dirs_exist_ok=True)
    shutil.copy2(access/'client_key.pub',payload/'guest-assets/guest-access.pub')
    (STATE/'assets/revision').write_text(hashlib.sha256(os.urandom(32)).hexdigest())
    return payload

def main():
    os.umask(0o077)
    if '--sync' in sys.argv: sync(); return
    proxy=STATE/'proxy'; downloads=STATE/'downloads'; downloads.mkdir(parents=True,exist_ok=True)
    if proxy.exists() and any(proxy.iterdir()): raise SystemExit('proxy directory already contains state; refusing to overwrite it (use --sync to update assets)')
    with tempfile.TemporaryDirectory(prefix='.proxy-prepare-',dir=STATE) as temporary:
        staged=Path(temporary)/'proxy';staged.mkdir()
        prepare(staged,downloads)
        if proxy.exists():proxy.rmdir()  # Only the empty directory checked above.
        staged.rename(proxy)
    print('Linux appliance prepared:',proxy)

def prepare(proxy, downloads):
    if not shutil.which('qemu-img'): raise SystemExit('qemu-img required: brew install qemu')
    manifest=json.loads((ROOT/'config/images.lock.json').read_text())['linux']
    image=downloads/'ubuntu.qcow2'; fetch(manifest['url'],image,manifest['sha256'])
    run('qemu-img','convert','-f','qcow2','-O','raw',image,proxy/'disk.raw')
    run('qemu-img','resize','-f','raw',proxy/'disk.raw','20G')
    sync()
    seed=proxy/'seed'; seed.mkdir(exist_ok=True)
    (seed/'meta-data').write_text('instance-id: warden-proxy-v1\nlocal-hostname: warden-proxy\n')
    (seed/'network-config').write_text('''version: 2
ethernets:
  wan0:
    match: {macaddress: "02:57:41:00:00:01"}
    set-name: wan0
    dhcp4: true
    dhcp6: false
    nameservers: {addresses: [1.1.1.1, 9.9.9.9]}
    dhcp4-overrides: {use-dns: false}
  lan0:
    match: {macaddress: "02:57:41:00:00:02"}
    set-name: lan0
    addresses: [10.77.0.1/24]
    dhcp4: false
    dhcp6: false
''')
    management = base64.b64encode((ROOT/'proxy/management.py').read_bytes()).decode()
    start_management = json.dumps(['bash','-c', 'if test -f /etc/systemd/system/warden-access.service; then systemctl start warden-access.service; else echo '+management+' | base64 -d > /run/warden-management.py; systemd-run --no-block --unit=warden-management /usr/bin/python3 /run/warden-management.py; fi'])
    (seed/'user-data').write_text('''#cloud-config
users: []
disable_root: true
ssh_pwauth: false
bootcmd:
  - MANAGEMENT_COMMAND
  - [mkdir, -p, /mnt/warden-assets]
  - [mount, -t, virtiofs, warden-assets, /mnt/warden-assets]
output: {all: '| tee -a /var/log/cloud-init-output.log /dev/hvc0'}
runcmd:
  - [bash, /mnt/warden-assets/payload/proxy/install.sh]
'''.replace('MANAGEMENT_COMMAND', start_management))
    run('hdiutil','makehybrid','-iso','-joliet','-default-volume-name','cidata','-o',proxy/'seed.iso',seed)

if __name__=='__main__': main()
