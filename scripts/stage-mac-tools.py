#!/usr/bin/env python3
"""Install unsigned-in app/tool files into a STOPPED macOS image, rootlessly.

No host Keychain or host Applications directory is modified. The MITM CA must
still be trusted from within the guest after Setup Assistant.
"""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import plistlib
import shutil
import subprocess
import sys
import tempfile

ROOT=Path(__file__).resolve().parents[1]; STATE=Path(os.environ.get('WARDEN_STATE',ROOT/'.local')).resolve()
def run(*args): return subprocess.run([str(a) for a in args],check=True,capture_output=True)
def install(data):
    sys.path.insert(0,str(ROOT/'host'))
    from warden.guest import initialize
    access=initialize(STATE)
    assets=STATE/'guest-assets'
    lock=json.loads((ROOT/'config/tools.lock.json').read_text())
    for name in ['gh.zip','Homebrew.pkg']:
        if hashlib.sha256((assets/name).read_bytes()).hexdigest()!=lock['artifacts'][name]['sha256']:
            raise RuntimeError('checksum mismatch: '+name)
    with tempfile.TemporaryDirectory(dir=STATE) as temporary:
        temp=Path(temporary)
        for name in ['Codex','Claude']:
            unpack=temp/name; unpack.mkdir()
            run('ditto','-xk',assets/(name+'.zip'),unpack)
            apps=list(unpack.glob('*.app'))
            if len(apps)!=1: raise RuntimeError('expected one application in '+name)
            run('codesign','--verify','--deep','--strict',apps[0])
            run('ditto',apps[0],data/'Applications'/apps[0].name)
            print('Installed',apps[0].name,flush=True)
        dest=data/'opt/warden'; dest.mkdir(parents=True,exist_ok=True)
        run('tar','-xzf',assets/'node.tar.gz','-C',dest)
        agents=dest/'agents'; agents.mkdir(exist_ok=True)
        run('tar','-xzf',assets/'agent-tools.tar.gz','-C',agents)
        node=next(dest.glob('node-v*-darwin-arm64'))
        binaries=data/'usr/local/bin'; binaries.mkdir(parents=True,exist_ok=True)
        for name in ['node','npm','npx']:
            link=binaries/name; link.unlink(missing_ok=True); link.symlink_to('/opt/warden/'+node.name+'/bin/'+name)
        wrappers={
            'codex':'/usr/local/bin/node /opt/warden/agents/node_modules/@openai/codex/bin/codex.js',
            'claude':'/opt/warden/agents/node_modules/@anthropic-ai/claude-code-darwin-arm64/claude'
        }
        for name,executable in wrappers.items():
            script=binaries/name
            script.write_text('#!/bin/sh\nexport NODE_EXTRA_CA_CERTS=/usr/local/share-warden-ca.pem\nexport SSL_CERT_FILE=/usr/local/share-warden-ca.pem\nexec '+executable+' "$@"\n')
            script.chmod(0o755)
        libexec=data/'usr/local/libexec';libexec.mkdir(parents=True,exist_ok=True)
        shutil.copy2(ROOT/'scripts/guest-bootstrap.sh',libexec/'warden-bootstrap.sh')
        shutil.copy2(ROOT/'scripts/install-guest-dev-tools.sh',libexec/'warden-install-dev-tools.sh')
        shutil.copy2(ROOT/'scripts/install-guest-access.sh',libexec/'warden-install-guest-access.sh')
        (libexec/'warden-install-guest-access.sh').chmod(0o755)
        (libexec/'warden-install-dev-tools.sh').chmod(0o755)
        installers=dest/'installers'; installers.mkdir(exist_ok=True)
        shutil.copy2(access/'client_key.pub',installers/'guest-access.pub')
        for name in ['gh.zip','Homebrew.pkg','DEVTOOLS-SHA256SUMS']:
            shutil.copy2(assets/name,installers/name)
        unpack=temp/'gh'; run('ditto','-xk',assets/'gh.zip',unpack)
        gh=list(unpack.glob('gh_*_macOS_arm64/bin/gh'))
        if len(gh)!=1: raise RuntimeError('expected one GitHub CLI binary')
        shutil.copy2(gh[0],binaries/'gh'); (binaries/'gh').chmod(0o755)
        shutil.copy2(ROOT/'scripts/warden-repo',binaries/'warden-repo'); (binaries/'warden-repo').chmod(0o755)
        shutil.copy2(assets/'warden-clipboard', libexec/'warden-clipboard')
        (libexec/'warden-clipboard').chmod(0o755)
        launchagents=data/'Library/LaunchAgents'; launchagents.mkdir(parents=True,exist_ok=True)
        (launchagents/'dev.warden.clipboard.plist').write_bytes(plistlib.dumps({
            'Label':'dev.warden.clipboard', 'ProgramArguments':['/usr/local/libexec/warden-clipboard'],
            'RunAtLoad':True, 'KeepAlive':True, 'ThrottleInterval':10, 'LimitLoadToSessionType':'Aqua'}))
        trust=binaries/'warden-trust-proxy'
        trust.write_text('''#!/bin/bash
set -euo pipefail
[[ "$(sysctl -n hw.model)" == VirtualMac* ]] || { echo 'Run this inside the Warden guest'; exit 1; }
if [[ "$EUID" != 0 ]]; then exec sudo /bin/bash "$0"; fi
curl -fsS http://10.77.0.1:8081/ca.pem -o /usr/local/share-warden-ca.pem
security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain /usr/local/share-warden-ca.pem
/usr/local/libexec/warden-install-dev-tools.sh --offline
/usr/local/libexec/warden-install-guest-access.sh --offline
echo 'Proxy CA trusted in the guest. Codex and Claude are ready to sign in.'
''')
        trust.chmod(0o755)
        paths=data/'private/etc/paths.d';paths.mkdir(parents=True,exist_ok=True);(paths/'warden').write_text('/usr/local/bin\n/opt/homebrew/bin\n/opt/homebrew/sbin\n')
        manifest={'installed_apps':[p.name for p in (data/'Applications').glob('*.app') if not p.is_symlink()], 'cli_packages':json.loads((ROOT/'config/tooling/package.json').read_text()),'signed_in':False,'ca_trust':'requires warden-trust-proxy inside guest'}
        manifest['github_cli']='2.100.0'
        manifest['guest_access']={'status':'staged; installed by warden-trust-proxy after first login','host_pairing':'requires verified guest fingerprint','private_key_in_image':False}
        manifest['homebrew']={'version':'6.0.22','status':'staged; warden-trust-proxy starts installation, waiting for Apple Command Line Tools if needed'}
        (STATE/'mac/tools-staged.json').write_text(json.dumps(manifest,indent=2)+'\n')
        print('CLI tools installed. Guest CA trust remains interactive.',flush=True)

def main():
    parser=argparse.ArgumentParser();parser.add_argument('--mounted-data',type=Path,help='Use an already mounted Data volume of the stopped guest (development only)');args=parser.parse_args()
    if args.mounted_data:
        if not (args.mounted_data/'private/var').is_dir() or not (args.mounted_data/'Applications').is_dir(): raise SystemExit('not a macOS Data volume')
        install(args.mounted_data);return
    with (STATE/'vm.lock').open('a') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        mounting=STATE/'mac-mount';mounting.mkdir(exist_ok=True)
        result=plistlib.loads(run('hdiutil','attach','-plist','-nobrowse','-owners','off','-mountroot',mounting,'-imagekey','diskimage-class=CRawDiskImage',STATE/'mac/disk.raw').stdout)
        entities=result['system-entities'];device=entities[0]['dev-entry']
        try:
            volumes=[Path(e['mount-point']) for e in entities if e.get('mount-point') and Path(e['mount-point']).name=='Data']
            if len(volumes)!=1: raise RuntimeError('cannot uniquely identify guest Data volume')
            install(volumes[0])
        finally: run('hdiutil','detach',device)
if __name__=='__main__':main()
