import os,sys,subprocess,tempfile,errno
from pathlib import Path
if sys.platform!='linux' or os.geteuid()!=0:raise SystemExit('Requires the disposable Linux appliance as root')
sys.path.insert(0,'/opt/warden/host')
from warden.core import Engine
with tempfile.TemporaryDirectory(prefix='warden-audit-full-',dir='/run') as temp:
 subprocess.run(['mount','-t','tmpfs','-o','size=1m','tmpfs',temp],check=True)
 engine=None
 try:
  engine=Engine(Path(temp)/'state')
  length=os.fstat(engine.audit.fd).st_size
  os.write(engine.audit.fd,b' '*(4096-length%4096));os.fsync(engine.audit.fd)
  with open(Path(temp)/'filler','wb',buffering=0) as stream:
   try:
    while True:stream.write(b'0'*16384)
   except OSError as error:assert error.errno==errno.ENOSPC
  try:engine.authorize_egress({'host':'api.openai.com','method':'POST','scheme':'https'})
  except OSError as error:assert error.errno==errno.ENOSPC
  else:raise AssertionError('disk-full authorization did not fail closed')
  print('Real full-filesystem test: authorization failed closed on ENOSPC.')
 finally:
  if engine:engine.db.close();os.close(engine.audit.fd)
  subprocess.run(['umount',temp],check=True)
