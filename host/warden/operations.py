"""Host-only operational commands and setup readiness."""
import argparse
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import signal
import time
import urllib.request

ROOT=Path(__file__).resolve().parents[2]

def stop_services(state):
    from .environments import stopped
    with stopped(state):
        for name,marker in [('menu','Warden Menu.app/Contents/MacOS/warden-vm'),('siem','warden.siem'),('control','warden.server')]:
            path=state/(name+'.pid')
            if not path.exists():continue
            pid=int(path.read_text())
            if pid<=1:raise ValueError('invalid '+name+' PID')
            result=subprocess.run(['ps','-p',str(pid),'-o','args='],capture_output=True,text=True)
            if result.returncode:continue
            if marker not in result.stdout or str(state) not in result.stdout:raise ValueError('refusing to stop an unrecognized '+name+' process')
            os.kill(pid,signal.SIGTERM)
            for _ in range(100):
                result=subprocess.run(['ps','-p',str(pid),'-o','stat='],capture_output=True,text=True)
                if result.returncode or result.stdout.strip().startswith('Z'):break
                time.sleep(.05)
            else:raise ValueError(name+' did not stop cleanly')
        import fcntl
        for name in ('menu','siem','control'):
            with (state/(name+'.lock')).open('a') as lock:
                try:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
                except BlockingIOError:raise ValueError('quit the '+name+' service for this environment before continuing')

def api(state,path,value=None):
    port=int((state/'control-port').read_text()) if (state/'control-port').exists() else 18765
    if not 1024<=port<=65535:raise ValueError('invalid host control port')
    origin='http://127.0.0.1:'+str(port)
    headers={'Authorization':'Bearer '+(state/'admin-token').read_text().strip(),'Origin':origin,'Content-Type':'application/json'}
    request=urllib.request.Request(origin+'/api/'+path,data=json.dumps(value).encode() if value is not None else None,headers=headers)
    # Never send host credentials through ambient proxy configuration.
    opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(request,timeout=10) as response:return json.load(response)

def readiness(state):
    checks=[]
    def add(label,ready,command):checks.append({'name':label,'ready':bool(ready),'next':None if ready else command})
    add('Apple Silicon macOS',platform.system()=='Darwin' and platform.machine()=='arm64','Use an Apple Silicon Mac')
    binaries=('python3','qemu-img','node','npm') if os.environ.get('WARDEN_PACKAGED') else ('swift','git','qemu-img','node','npm')
    for binary in binaries:
        add(binary,shutil.which(binary),'xcode-select --install' if binary in ('swift','git') else 'brew install '+('qemu' if binary=='qemu-img' else 'node'))
    add('Free disk space (70 GiB)',shutil.disk_usage(state).free>=70*1024**3,'Free space for VM disks and recovery checkpoints')
    add('Native launcher',(state/'bin/warden-vm').exists(),'./warden build')
    add('Linux appliance',(state/'proxy/disk.raw').exists(),'./warden prepare')
    add('macOS image',(state/'mac/installed').exists(),'./warden install-mac')
    add('Guest access paired',(state/'guest/connection.json').exists(),'Complete guest Setup Assistant, run sudo warden-trust-proxy, then ./warden guest pair')
    guest_ready=False
    if (state/'guest/connection.json').exists():
        try:
            from .guest import ssh_options
            result=subprocess.run(['/usr/bin/ssh',*ssh_options(state),'-T','warden-guest','command -v codex >/dev/null && command -v gh >/dev/null && command -v brew >/dev/null && warden-repo doctor >/dev/null'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=8)
            guest_ready=result.returncode==0
        except (OSError,ValueError,subprocess.TimeoutExpired):pass
    add('Guest developer tools and proxy CA',guest_ready,'./warden guest exec -- warden-repo doctor')
    try:status=api(state,'state')
    except Exception:status={}
    add('Host control plane',bool(status),'./warden control')
    add('Proxy enforcing',status.get('proxy_ready'),'./warden run')
    add('Restricted egress policy',status.get('policy',{}).get('egress',{}).get('mode')=='restricted','./warden admin policy restricted')
    return {'checks':checks,'ready':all(c['ready'] for c in checks)}

def main(state,argv):
    parser=argparse.ArgumentParser(description='Warden host operations')
    subs=parser.add_subparsers(dest='command',required=True)
    admin=subs.add_parser('admin');actions=admin.add_subparsers(dest='action',required=True)
    network=actions.add_parser('network');network.add_argument('mode',choices=['on','off'])
    actions.add_parser('revoke-all')
    policy=actions.add_parser('policy');policy.add_argument('mode',choices=['restricted','public']);policy.add_argument('--file',type=Path)
    setup=subs.add_parser('setup');setup.add_argument('--apply',action='store_true')
    setup.add_argument('--json',action='store_true')
    services=subs.add_parser('services');services.add_argument('action',choices=['stop'])
    args=parser.parse_args(argv)
    if args.command=='services':stop_services(state);print('Host services stopped. VM state retained.');return 0
    if args.command=='admin':
        if args.action=='network':result=api(state,'network',{'enabled':args.mode=='on'})
        elif args.action=='revoke-all':result=api(state,'revoke-all',{})
        else:
            current=api(state,'state')['policy']
            current['egress']=json.loads((args.file or ROOT/'config/egress.restricted.json').read_text()) if args.mode=='restricted' else {'mode':'public','destinations':[]}
            if current['egress']['mode']!=args.mode:raise ValueError('policy file mode mismatch')
            result=api(state,'policy',current)
        print(json.dumps(result,indent=2));return 0
    status=readiness(state)
    if args.json:print(json.dumps(status,indent=2));return 0 if status['ready'] else 1
    for check in status['checks']:print(('Ready: ' if check['ready'] else 'Next: ')+check['name']+(' — '+check['next'] if check['next'] else ''))
    if args.apply:
        if any(not check['ready'] for check in status['checks'] if check['name'] in ('Apple Silicon macOS','swift','git','python3','qemu-img','node','npm','Free disk space (70 GiB)')):raise ValueError('install missing host prerequisites first')
        for command,ready in [('build',(state/'bin/warden-vm').exists()),('prepare',(state/'proxy/disk.raw').exists()),('install-mac',(state/'mac/installed').exists())]:
            if not ready:subprocess.run([str(ROOT/'warden'),command],check=True,env={**os.environ,'WARDEN_STATE':str(state)})
        subprocess.run([str(ROOT/'warden'),'control'],check=True,env={**os.environ,'WARDEN_STATE':str(state)})
        print('Prepared. Run ./warden run, complete guest Setup Assistant, and run sudo warden-trust-proxy once in the guest.')
    return 0
