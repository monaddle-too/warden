"""Offline desktop setup using explicitly selected, checksum-pinned inputs."""
import argparse
import fcntl
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import time
import tempfile
from .environments import digest, stopped

ROOT = Path(__file__).resolve().parents[2]


def manifest():
    return json.loads((ROOT/'config/offline-inputs.json').read_text())


def initialize(state):
    state.mkdir(parents=True, exist_ok=True, mode=0o700)
    if len(str(state/'guest/relay.sock').encode()) > 100:
        raise ValueError('State path too long for private management sockets; use a shorter WARDEN_STATE path')
    runtime = ROOT/'runtime'
    for name in ('Warden.app', 'Warden Menu.app', 'bin'):
        dest = state/name; source = runtime/name
        if not source.exists(): raise ValueError('Incomplete app: missing '+name)
        if dest.is_symlink() and dest.resolve() == source.resolve(): continue
        if dest.exists() and not dest.is_symlink(): raise ValueError('Existing state contains a different runtime; use a separate state directory')
        with stopped(state, control=True):
            temp = state/('.'+name+'.new')
            temp.unlink(missing_ok=True); temp.symlink_to(source, target_is_directory=True); os.replace(temp,dest)
    assets = state/'guest-assets'; assets.mkdir(exist_ok=True)
    target = assets/'warden-clipboard'
    source = runtime/'guest-assets/warden-clipboard'
    if target.is_symlink() and target.resolve() == source.resolve(): return
    if target.exists() and not target.is_symlink(): raise ValueError('Existing clipboard runtime must be migrated separately')
    with stopped(state, control=True):
        temp=assets/'.clipboard.new';temp.unlink(missing_ok=True);temp.symlink_to(source);os.replace(temp,target)


def held(path):
    with path.open('a') as lock:
        try: fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB); return False
        except BlockingIOError:return True


def status(state):
    inputs=manifest()['files']
    missing=[name for name in inputs if not (state/name).is_file()]
    # Validation itself is deliberately a separate long-running action.
    try: ready=json.loads((state/'inputs-verified.json').read_text())['manifest_sha256']==digest(ROOT/'config/offline-inputs.json') and not missing
    except (OSError,ValueError,KeyError): ready=False
    state_busy=held(state/'vm.lock')
    running=False
    try:
        record=json.loads((state/'vm-status.json').read_text());pid=int(record['pid'])
        process=subprocess.run(['ps','-p',str(pid),'-o','args='],capture_output=True,text=True)
        running=state_busy and pid>1 and str(state/'bin') in process.stdout
        if state_busy and pid>1 and str(state/'Warden.app') in process.stdout:running=True
    except (OSError,ValueError,KeyError):pass
    return {'state':str(state), 'local_only':True, 'inputs_ready':ready, 'missing':missing,
            'proxy_prepared':all((state/'proxy'/n).is_file() for n in ('disk.raw','seed.iso')), 'mac_installed':(state/'mac/installed').is_file(),
            'tools_staged':(state/'mac/tools-staged.json').is_file(), 'paired':(state/'guest/connection.json').is_file(), 'vm_running':running, 'state_busy':state_busy,
            'free_gib':round(shutil.disk_usage(state).free/1024**3,1),
            'controller_conflict':controller_conflict(state)}


def controller_conflict(state):
    if held(state/'control.lock'): return False
    with socket.socket() as sock:
        sock.settimeout(.2)
        return sock.connect_ex(('127.0.0.1',18765)) == 0


def verify_inputs(state):
    for name, expected in manifest()['files'].items():
        path=state/name
        print('Verifying '+Path(name).name,flush=True)
        if not path.is_file() or digest(path)!=expected['sha256']:
            raise ValueError('Missing or mismatched input: '+name)
    (state/'inputs-verified.json').write_text(json.dumps({'manifest_sha256':digest(ROOT/'config/offline-inputs.json')}))


def import_inputs(state, source):
    source=source.resolve()
    with stopped(state), tempfile.TemporaryDirectory(prefix='.import-',dir=state) as temp:
        for name, expected in manifest()['files'].items():
            path=source/name
            if not path.is_file():raise ValueError('Selected folder is missing '+name)
            print('Importing '+Path(name).name,flush=True)
            # Copy before verifying, so changing a selected input cannot race its check.
            target=Path(temp)/Path(name).name
            subprocess.run(['/bin/cp','-c',str(path),str(target)],check=True)
            target.chmod(0o600)
            if digest(target)!=expected['sha256']:raise ValueError('Checksum mismatch: '+name)
            destination=state/name;destination.parent.mkdir(parents=True,exist_ok=True)
            os.replace(target,destination)
        (state/'inputs-verified.json').write_text(json.dumps({'manifest_sha256':digest(ROOT/'config/offline-inputs.json')}))


def execute(state, command):
    env={**os.environ,'WARDEN_STATE':str(state),'WARDEN_OFFLINE':'1'}
    subprocess.run([sys.executable,str(ROOT/'warden'),command],env=env,check=True)


def discover_inputs(state):
    # Inspect only conventional installer locations, never scan user projects.
    candidates=[state, Path.home()/'Library/Caches/Warden', ROOT.parents[3]/'Warden Inputs']
    for candidate in candidates:
        if all((candidate/name).is_file() for name in manifest()['files']):return candidate
    return None


def setup(state, source=None, prepare_only=False):
    with (state/'desktop-setup.lock').open('a') as lock:
        try:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError:raise ValueError('Setup is already running')
        with stopped(state):
            if shutil.disk_usage(state).free<70*1024**3:raise ValueError('At least 70 GiB free disk space required')
        def step(name):print(json.dumps({'setup_step':name}),flush=True)
        if not status(state)['inputs_ready']:
            source=source or discover_inputs(state)
            if source is None:raise ValueError('Choose your local installer folder to continue')
            step('importing')
            if source.resolve()!=state.resolve():import_inputs(state,source)
        step('checking');verify_inputs(state)
        if not all((state/'proxy'/n).is_file() for n in ('disk.raw','seed.iso')):
            step('preparing')
            with stopped(state):execute(state,'prepare')
        if not prepare_only:
            if not (state/'mac/installed').is_file():
                step('installing');execute(state,'install-mac')
            elif not (state/'mac/tools-staged.json').is_file():
                step('tools');execute(state,'stage-tools')
        step('prepared' if prepare_only else 'ready')


def main(state, argv):
    parser=argparse.ArgumentParser(description='Warden offline desktop setup')
    parser.add_argument('command',choices=['status','import','verify','prepare','install','start','dashboard','setup'])
    parser.add_argument('source',nargs='?',type=Path)
    parser.add_argument('--prepare-only',action='store_true')
    args=parser.parse_args(argv[1:])
    initialize(state)
    if args.command=='status':
        value=status(state);source=discover_inputs(state);value['suggested_inputs']=str(source) if source else None
        print(json.dumps(value));return 0
    if args.command=='setup':setup(state,args.source,args.prepare_only);return 0
    if args.command=='import':
        if args.source is None:raise ValueError('Choose the local installer cache folder')
        import_inputs(state,args.source)
    elif args.command=='verify':verify_inputs(state)
    elif args.command=='prepare':
        with stopped(state):
            if shutil.disk_usage(state).free<70*1024**3:raise ValueError('At least 70 GiB free disk space required')
            verify_inputs(state);execute(state,'prepare')
    elif args.command=='install':
        verify_inputs(state)
        if not all((state/'proxy'/n).is_file() for n in ('disk.raw','seed.iso')):raise ValueError('Prepare the local images first')
        execute(state,'install-mac')
    elif args.command in ('start','dashboard'):
        if controller_conflict(state):raise ValueError('Another Warden environment is active on this Mac. Shut it down before starting this one.')
        if args.command=='start':
            if held(state/'vm.lock'):raise ValueError('This environment is already running')
            with (state/'desktop-vm.log').open('ab',buffering=0) as log:
                process=subprocess.Popen([sys.executable,str(ROOT/'warden'),'run'],env={**os.environ,'WARDEN_STATE':str(state)},stdin=subprocess.DEVNULL,stdout=log,stderr=log,start_new_session=True)
            (state/'desktop-vm.pid').write_text(str(process.pid));time.sleep(1)
            if process.poll() is not None:raise ValueError('VM launcher exited; inspect desktop-vm.log')
            print('Environment starting. Complete guest Setup Assistant in its window.')
        else:execute(state,'control');execute(state,'open')
    return 0
