#!/usr/bin/env python3
"""Repeatable validation; disruptive probes require a separate Linux-only VM."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import time

ROOT=Path(__file__).resolve().parents[1]

def main():
    parser=argparse.ArgumentParser();parser.add_argument('--appliance',type=Path);parser.add_argument('--skip-unit',action='store_true');args=parser.parse_args()
    state=Path(os.environ.get('WARDEN_STATE',ROOT/'.local')).resolve();out=state/'validation'/time.strftime('%Y%m%d-%H%M%S');out.mkdir(parents=True)
    results=[]
    def check(label,command,env=None,timeout=240):
        with (out/(label+'.log')).open('w') as log:
            result=subprocess.run(command,cwd=ROOT,env=env,stdout=log,stderr=subprocess.STDOUT,timeout=timeout)
        results.append({'check':label,'passed':result.returncode==0,'log':str(out/(label+'.log'))});print(label,':','PASS' if result.returncode==0 else 'FAIL',flush=True)
    if not args.skip_unit:check('regression',[str(ROOT/'warden'),'test'])
    if args.appliance:
        appliance=args.appliance.resolve()
        if appliance==state or (appliance/'mac/disk.raw').exists():raise SystemExit('Use a separate Linux-only validation appliance; never the development VM pair')
        env={**os.environ,'WARDEN_STATE':str(appliance)}
        check('network-boundary',['python3','scripts/proxy-exec.py','bash','/opt/warden/tests/network-smoke.sh','--disposable'],env)
        script=(ROOT/'scripts/diagnose-netfilter.py').read_text()
        check('kernel-pressure',['python3','scripts/proxy-exec.py','python3','-c',script,'--seconds','30','--memory-mib','1024','--workers','4'],env)
        check('audit-disk-full',['python3','scripts/proxy-exec.py','python3','-c',(ROOT/'tests/disposable-disk-full.py').read_text()],env)
        check('package-integrity',['python3','scripts/proxy-exec.py','bash','-c','test "$(cat /proc/sys/kernel/tainted)" = 0 && test -z "$(dpkg -V)"'],env)
        disk_probe='''import hashlib,os,tempfile
from pathlib import Path
block=os.urandom(1024*1024);expected=hashlib.sha256(block*64).hexdigest()
with tempfile.TemporaryDirectory(prefix="warden-storage-",dir="/var/tmp") as temp:
 path=Path(temp)/"probe"
 for iteration in range(5):
  with path.open("wb") as stream:
   for _ in range(64):stream.write(block)
   stream.flush();os.fsync(stream.fileno())
  assert hashlib.sha256(path.read_bytes()).hexdigest()==expected
  Path("/proc/sys/vm/drop_caches").write_text("3")
  assert hashlib.sha256(path.read_bytes()).hexdigest()==expected
 print("Five 64 MiB durable write/read/cache-drop rounds passed")
'''
        check('storage-integrity',['python3','scripts/proxy-exec.py','python3','-c',disk_probe],env)
    report={'time':time.time(),'checks':results,'passed':all(r['passed'] for r in results),'limits':['Linux root namespace probes do not establish immunity to a hypervisor exploit.','Historic kernel/storage anomalies remain open until a cause and fix are established.']}
    (out/'report.json').write_text(json.dumps(report,indent=2)+'\n');print(out/'report.json')
    return 0 if report['passed'] else 1
if __name__=='__main__':raise SystemExit(main())
