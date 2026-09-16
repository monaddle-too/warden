"""Signed release manifests and atomic installations with separate VM state."""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import stat
import subprocess
import tempfile
import time
import uuid
import zipfile
from .environments import digest, stopped

ROOT=Path(__file__).resolve().parents[2]
NAMESPACE='warden-release'

def run(*args,**kwargs):return subprocess.run([str(a) for a in args],check=True,**kwargs)

def keygen(state):
    folder=state/'release-signing';folder.mkdir(mode=0o700,exist_ok=True);key=folder/'key'
    if not key.exists():run('/usr/bin/ssh-keygen','-q','-t','ed25519','-N','','-C','warden-local-releases','-f',key)
    key.chmod(0o600);return key

def verify(archive,manifest,signature,public_key):
    if manifest.stat().st_size>1024*1024 or signature.stat().st_size>16384:raise ValueError('release metadata too large')
    key=' '.join(public_key.read_text().split()[:2])
    from .guest import key_fingerprint
    key_fingerprint(key)
    with tempfile.TemporaryDirectory(prefix='warden-release-verify-') as temp:
        allowed=Path(temp)/'allowed';allowed.write_text('warden '+key+'\n')
        result=subprocess.run(['/usr/bin/ssh-keygen','-Y','verify','-f',str(allowed),'-I','warden','-n',NAMESPACE,'-s',str(signature)],input=manifest.read_bytes(),capture_output=True)
        if result.returncode:raise ValueError('release signature verification failed')
    data=json.loads(manifest.read_text())
    if data.get('version')!=1 or not re.fullmatch(r'[a-f0-9]{64}',data.get('sha256','')):raise ValueError('invalid release manifest')
    if archive.stat().st_size!=data.get('bytes') or digest(archive)!=data['sha256']:raise ValueError('release archive checksum mismatch')
    return data

def extract(archive,directory):
    total=0
    with zipfile.ZipFile(archive) as source:
        entries=source.infolist()
        if len(entries)>20000:raise ValueError('too many release entries')
        seen=set()
        for entry in entries:
            parts=PurePosixPath(entry.filename).parts
            if parts and parts[0]=='__MACOSX':continue
            if not parts or parts[0]!='warden' or any(p in ('..','') for p in parts) or entry.filename.startswith('/') or '\\' in entry.filename:raise ValueError('unsafe archive path')
            normalized='/'.join(parts)
            if normalized in seen:raise ValueError('duplicate archive path')
            seen.add(normalized)
            total+=entry.file_size
            if total>2*1024**3:raise ValueError('release expands beyond size limit')
            target=directory.joinpath(*parts)
            mode=entry.external_attr>>16
            if stat.S_ISLNK(mode):
                # The distribution has one expected relative launcher link.
                value=source.read(entry).decode()
                if normalized!='warden/.local/bin/warden-vm' or value!='../Warden.app/Contents/MacOS/warden-vm':raise ValueError('unexpected archive symlink')
                target.parent.mkdir(parents=True,exist_ok=True);target.symlink_to(value)
            elif entry.is_dir():target.mkdir(parents=True,exist_ok=True)
            elif stat.S_IFMT(mode) not in (0,stat.S_IFREG):raise ValueError('unsupported archive file')
            else:
                target.parent.mkdir(parents=True,exist_ok=True)
                with source.open(entry) as src,target.open('xb') as dest:
                    shutil.copyfileobj(src,dest);dest.flush();os.fsync(dest.fileno())
                target.chmod(0o755 if mode & 0o111 else 0o644)
    root=directory/'warden'
    for required in ['warden','host/warden/core.py','.local/Warden.app/Contents/MacOS/warden-vm','.local/Warden Menu.app/Contents/MacOS/warden-vm']:
        if not (root/required).is_file():raise ValueError('incomplete Warden release')
    return root

def switch(prefix,release):
    current=prefix/'current'
    if current.is_symlink():
        previous=prefix/'.previous.new';previous.unlink(missing_ok=True);previous.symlink_to(os.readlink(current));os.replace(previous,prefix/'previous')
    temp=prefix/'.current.new';temp.unlink(missing_ok=True);temp.symlink_to(release.relative_to(prefix));os.replace(temp,current)
    fd=os.open(prefix,os.O_RDONLY)
    try:os.fsync(fd)
    finally:os.close(fd)

def install(prefix,archive,manifest,signature,public_key=None):
    prefix=prefix.resolve();existing=(prefix/'install.json').is_file()
    if prefix.exists() and not existing and any(prefix.iterdir()):raise ValueError('install into a new empty directory; the source checkout is never replaced')
    trust=prefix/'trusted-release.pub' if existing else public_key
    if trust is None:raise ValueError('first installation requires an explicitly trusted public key')
    metadata=verify(archive,manifest,signature,trust)
    prefix.mkdir(parents=True,exist_ok=True,mode=0o700)
    state=prefix/'state';state.mkdir(exist_ok=True,mode=0o700)
    if existing:
        from .operations import stop_services
        stop_services(state)
    with stopped(state,control=True):
        if existing:
            previous=json.loads((prefix/'current/release.json').read_text())
            if metadata['created']<=previous['created']:raise ValueError('release is not newer; use rollback for an intentional downgrade')
        releases=prefix/'releases';releases.mkdir(exist_ok=True)
        target=releases/metadata['sha256'][:20]
        if target.exists():raise ValueError('release already staged')
        with tempfile.TemporaryDirectory(prefix='.stage-',dir=releases) as temp:
            root=extract(archive,Path(temp))
            if os.uname().sysname=='Darwin':
                for app in ('Warden.app','Warden Menu.app'):run('/usr/bin/codesign','--verify','--deep','--strict',root/'.local'/app)
            (root/'.local').rename(root/'runtime');(root/'.local').symlink_to(state,target_is_directory=True)
            root.rename(target)
        if not existing:
            shutil.copyfile(trust,prefix/'trusted-release.pub')
            for part in ['Warden.app','Warden Menu.app','bin']:(state/part).symlink_to(prefix/'current/runtime'/part,target_is_directory=True)
            (state/'guest-assets').mkdir()
            (state/'guest-assets/warden-clipboard').symlink_to(prefix/'current/runtime/guest-assets/warden-clipboard')
            (prefix/'bin').mkdir()
            wrapper=prefix/'bin/warden'
            wrapper.write_text('#!/bin/sh\nbase=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)\nexport WARDEN_STATE="$base/state"\nexec "$base/current/warden" "$@"\n');wrapper.chmod(0o755)
        shutil.copyfile(manifest,target/'release.json')
        switch(prefix,target)
        temp=prefix/'install.json.tmp';temp.write_text(json.dumps(metadata,indent=2)+'\n');os.replace(temp,prefix/'install.json')
    return prefix/'bin/warden'

def rollback(prefix):
    from .operations import stop_services
    stop_services(prefix/'state')
    with stopped(prefix/'state',control=True):
        previous=prefix/'previous'
        if not previous.is_symlink():raise ValueError('no previous release')
        release=previous.resolve()
        if release.parent!=(prefix/'releases').resolve():raise ValueError('invalid previous release')
        metadata=json.loads((release/'release.json').read_text())
        switch(prefix,release)
        (prefix/'install.json').write_text(json.dumps(metadata,indent=2)+'\n')

def build(state,identity=None,profile=None):
    if bool(identity)!=bool(profile):raise ValueError('supply both Apple signing identity and notary profile')
    run('bash',ROOT/'scripts/build-native.sh')
    if identity:
        if not identity.startswith('Developer ID Application:'):raise ValueError('Developer ID Application identity required')
        run('/usr/bin/codesign','--force','--sign',identity,'--options','runtime','--timestamp','--entitlements',ROOT/'native/warden.entitlements',state/'Warden.app')
        for target in [state/'Warden Menu.app',state/'guest-assets/warden-clipboard']:
            run('/usr/bin/codesign','--force','--sign',identity,'--options','runtime','--timestamp',target)
    run('bash',ROOT/'scripts/package.sh',env={**os.environ,'WARDEN_SKIP_NATIVE_BUILD':'1'})
    archive=ROOT/'dist/warden-0.1.0-arm64.zip'
    if identity:
        run('xcrun','notarytool','submit',archive,'--keychain-profile',profile,'--wait')
        for app in ['Warden.app','Warden Menu.app']:run('xcrun','stapler','staple',state/app)
        run('bash',ROOT/'scripts/package.sh',env={**os.environ,'WARDEN_SKIP_NATIVE_BUILD':'1'})
    metadata={'version':1,'created':time.time(),'sha256':digest(archive),'bytes':archive.stat().st_size,'apple_notarized':bool(identity),'components':{p.name:json.loads(p.read_text()) for p in [ROOT/'config/images.lock.json',ROOT/'config/tools.lock.json',ROOT/'config/tooling/package-lock.json']},'python_requirements_sha256':digest(ROOT/'proxy/requirements.lock')}
    manifest=archive.with_suffix('.manifest.json');manifest.write_text(json.dumps(metadata,indent=2)+'\n')
    signature=Path(str(manifest)+'.sig');signature.unlink(missing_ok=True)
    key=keygen(state);run('/usr/bin/ssh-keygen','-Y','sign','-f',key,'-n',NAMESPACE,manifest)
    print('Signed release:',manifest,'\nTrusted public key:',str(key)+'.pub','\nApple notarized:',bool(identity))

def main(state,argv):
    parser=argparse.ArgumentParser(description='Signed releases, atomic updates and reversible uninstall')
    subs=parser.add_subparsers(dest='command',required=True)
    release=subs.add_parser('release');actions=release.add_subparsers(dest='action',required=True)
    actions.add_parser('keygen');child=actions.add_parser('build');child.add_argument('--apple-identity');child.add_argument('--notary-profile')
    update=subs.add_parser('update');actions=update.add_subparsers(dest='action',required=True)
    for action in ['install','apply','verify']:
        child=actions.add_parser(action);child.add_argument('--archive',type=Path,required=True);child.add_argument('--manifest',type=Path,required=True);child.add_argument('--signature',type=Path,required=True);child.add_argument('--key',type=Path);child.add_argument('--prefix',type=Path,required=True)
    for action in ['rollback','uninstall']:
        child=actions.add_parser(action);child.add_argument('--prefix',type=Path,required=True)
    args=parser.parse_args(argv)
    if args.command=='release':
        if args.action=='keygen':print(str(keygen(state))+'.pub')
        else:build(state,args.apple_identity,args.notary_profile)
    elif args.action=='rollback':rollback(args.prefix.resolve());print('Previous verified release restored; VM state retained.')
    elif args.action=='uninstall':
        prefix=args.prefix.resolve()
        if not (prefix/'install.json').exists():raise ValueError('not a managed installation')
        from .operations import stop_services
        stop_services(prefix/'state')
        with stopped(prefix/'state',control=True):
            destination=prefix.with_name(prefix.name+'.uninstalled-'+uuid.uuid4().hex[:8]);prefix.rename(destination)
        print('Installation removed from its active location; recoverable state:',destination)
    elif args.action=='verify':print(json.dumps(verify(args.archive,args.manifest,args.signature,args.key or args.prefix/'trusted-release.pub'),indent=2))
    else:
        if args.action=='apply' and not (args.prefix/'install.json').is_file():raise ValueError('not a managed installation')
        print('Installed launcher:',install(args.prefix,args.archive,args.manifest,args.signature,args.key))
    return 0
