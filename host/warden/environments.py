"""Independent project states and verified, stopped-disk recovery."""
import argparse
from contextlib import contextmanager
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import time
import uuid

ROOT=Path(__file__).resolve().parents[2]
NAME=re.compile(r'[a-z][a-z0-9-]{0,23}')
FILES=('mac/disk.raw','mac/hardware.bin','mac/machine.bin','mac/auxiliary.bin','mac/installed','proxy/disk.raw','proxy/efi.bin','proxy/seed.iso')

def name(value):
    if not NAME.fullmatch(value):raise ValueError('use a lowercase name, up to 24 letters, digits or hyphens')
    return value

@contextmanager
def stopped(state,control=False):
    state.mkdir(parents=True,exist_ok=True,mode=0o700)
    with (state/'vm.lock').open('a') as vm:
        try:fcntl.flock(vm,fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError:raise ValueError('shut down the guest cleanly before changing its disk')
        if control:
            with (state/'control.lock').open('a') as service:
                try:fcntl.flock(service,fcntl.LOCK_EX|fcntl.LOCK_NB)
                except BlockingIOError:raise ValueError('stop this environment control plane before restoring it')
                yield
        else:yield

def digest(path):
    h=hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda:stream.read(8*1024*1024),b''):h.update(chunk)
    return h.hexdigest()

def copy_disk(source,target):
    target.parent.mkdir(parents=True,exist_ok=True)
    if os.uname().sysname=='Darwin':subprocess.run(['/bin/cp','-c',str(source),str(target)],check=True)
    else:shutil.copyfile(source,target)
    target.chmod(0o600)
    with target.open('rb') as stream:os.fsync(stream.fileno())

def capture(state,label):
    label=name(label);snapshots=state/'checkpoints';snapshots.mkdir(exist_ok=True,mode=0o700)
    target=snapshots/label
    if target.exists():raise ValueError('checkpoint already exists')
    with stopped(state),tempfile.TemporaryDirectory(prefix='.capture-',dir=snapshots) as temp:
        stage=Path(temp);records={}
        for relative in FILES:
            source=state/relative
            if source.is_file():
                print('Checkpoint:',relative,flush=True)
                copy_disk(source,stage/relative)
                records[relative]={'sha256':digest(stage/relative),'bytes':source.stat().st_size}
        if 'mac/disk.raw' not in records or 'proxy/disk.raw' not in records:raise ValueError('both VM disks must exist')
        manifest={'version':1,'created':time.time(),'files':records,'machine_sha256':digest(state/'mac/machine.bin')}
        (stage/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
        stage.rename(target)
    return target

def verify(checkpoint):
    manifest=json.loads((checkpoint/'manifest.json').read_text())
    if manifest.get('version')!=1 or not isinstance(manifest.get('files'),dict) or set(manifest['files'])-set(FILES):raise ValueError('invalid checkpoint manifest')
    if not {'mac/disk.raw','proxy/disk.raw','mac/machine.bin'}<=set(manifest['files']):raise ValueError('incomplete checkpoint')
    for relative,entry in manifest['files'].items():
        path=checkpoint/relative
        if path.is_symlink() or not path.is_file() or path.stat().st_size!=entry['bytes'] or digest(path)!=entry['sha256']:raise ValueError('checkpoint corrupt: '+relative)
    return manifest

def restore(state,label):
    checkpoint=state/'checkpoints'/name(label)
    with stopped(state,control=True):
        if (state/'restore-journal.json').exists():raise ValueError('an interrupted restore needs recovery first: warden recovery recover')
        manifest=verify(checkpoint)
        if digest(state/'mac/machine.bin')!=manifest['machine_sha256']:raise ValueError('checkpoint belongs to another VM identity')
        # Prepare all copies before changing any live file. Retain the original
        # files until the transaction commits; recovery always rolls back.
        transaction=state/('.restore-'+uuid.uuid4().hex);transaction.mkdir(mode=0o700)
        for relative in manifest['files']:copy_disk(checkpoint/relative,transaction/'new'/relative)
        journal={'directory':transaction.name,'files':list(manifest['files'])}
        path=state/'restore-journal.json';path.write_text(json.dumps(journal));sync_file(path);sync_directory(state)
        try:
            for relative in journal['files']:
                original=state/relative;backup=transaction/'old'/relative;backup.parent.mkdir(parents=True,exist_ok=True)
                if original.exists():original.rename(backup)
                sync_directory(backup.parent);sync_directory(original.parent)
                (transaction/'new'/relative).rename(original)
                sync_directory(original.parent)
            # Policy, GitHub credentials, approvals, SSH client identity and audit
            # are never restored from a disk checkpoint. Engine startup revokes
            # all old grants. Persist disconnect until explicit host reconnect.
            (state/'network-disconnected').write_text('restored; reconnect explicitly\n')
            sync_file(state/'network-disconnected')
            path.unlink();sync_directory(state);shutil.rmtree(transaction)
        except BaseException:
            rollback(state);raise
    return checkpoint

def sync_file(path):
    with path.open('rb') as stream:os.fsync(stream.fileno())

def sync_directory(path):
    fd=os.open(path,os.O_RDONLY)
    try:os.fsync(fd)
    finally:os.close(fd)

def rollback(state):
    path=state/'restore-journal.json';journal=json.loads(path.read_text())
    directory=journal['directory']
    if not re.fullmatch(r'\.restore-[a-f0-9]{32}',directory) or set(journal['files'])-set(FILES):raise ValueError('invalid restore journal')
    transaction=state/directory
    for relative in journal['files']:
        backup=transaction/'old'/relative
        if backup.exists():os.replace(backup,state/relative)
    path.unlink();shutil.rmtree(transaction)

def project_create(state,label,repositories):
    from .egress import REPO
    from .guest import initialize
    name(label)
    if not repositories or any(not REPO.fullmatch(r) for r in repositories):raise ValueError('supply at least one OWNER/REPO')
    projects=state/'projects';projects.mkdir(exist_ok=True,mode=0o700);target=projects/label
    if len(str(target/'vm-management.sock').encode())>100:raise ValueError('project state path too long for private sockets; use a shorter project name or WARDEN_STATE root')
    target.mkdir(mode=0o700)
    policy=json.loads((ROOT/'config/policy.template.json').read_text())
    policy['allowed_repositories']=repositories
    policy['egress']=json.loads((ROOT/'config/egress.restricted.json').read_text())
    (target/'policy.json').write_text(json.dumps(policy,indent=2)+'\n')
    (target/'project.json').write_text(json.dumps({'name':label,'repositories':repositories,'created':time.time(),'image':'fresh provisioning required'})+'\n')
    initialize(target)
    return target

def main(state,argv):
    parser=argparse.ArgumentParser(description='Independent projects and stopped-disk checkpoints')
    commands=parser.add_subparsers(dest='command',required=True)
    project=commands.add_parser('project');actions=project.add_subparsers(dest='action',required=True)
    create=actions.add_parser('create');create.add_argument('name');create.add_argument('--repo',action='append',required=True)
    actions.add_parser('list')
    run=actions.add_parser('run');run.add_argument('name');run.add_argument('args',nargs=argparse.REMAINDER)
    archive=actions.add_parser('archive');archive.add_argument('name')
    recovery=commands.add_parser('recovery');actions=recovery.add_subparsers(dest='action',required=True)
    for action in ('capture','verify','restore'):
        child=actions.add_parser(action);child.add_argument('name')
    actions.add_parser('list');actions.add_parser('recover')
    args=parser.parse_args(argv)
    if args.command=='project':
        directory=state/'projects'
        if args.action=='create':print(project_create(state,args.name,args.repo));return 0
        if args.action=='list':
            for p in sorted(directory.glob('*/project.json')):print(p.parent.name,json.loads(p.read_text())['repositories'])
            return 0
        target=directory/name(args.name)
        if not (target/'project.json').is_file():raise ValueError('unknown project')
        if args.action=='archive':
            from .operations import stop_services
            stop_services(target)
            with stopped(target,control=True):
                archived=state/'archived-projects';archived.mkdir(exist_ok=True,mode=0o700);target.rename(archived/(args.name+'-'+uuid.uuid4().hex[:8]))
            print('Project archived with its disks; no credentials were copied to another project.');return 0
        command=args.args[1:] if args.args[:1]==['--'] else args.args
        if not command:raise ValueError('supply a warden command after --')
        if command[0] in ('run','control','proxy'):
            # The current desktop has one approval endpoint, so only one project
            # control plane may run at a time. Never mix a new VM with another
            # project's controller.
            import socket
            with socket.socket() as sock:
                if sock.connect_ex(('127.0.0.1',18765))==0:
                    from .operations import api
                    try:api(target,'state')
                    except Exception:raise ValueError('another project owns port 18765; stop it before starting this project')
        return subprocess.run([str(ROOT/'warden'),*command],env={**os.environ,'WARDEN_STATE':str(target)}).returncode
    if args.action=='capture':print(capture(state,args.name))
    elif args.action=='verify':print(json.dumps(verify(state/'checkpoints'/name(args.name)),indent=2))
    elif args.action=='restore':print('Restored',restore(state,args.name),'with network disconnected and grants revoked on startup.')
    elif args.action=='recover':
        with stopped(state,control=True):rollback(state)
    else:
        for p in sorted((state/'checkpoints').glob('*/manifest.json')):print(p.parent.name,json.loads(p.read_text())['created'])
    return 0
