"""Persistent cutoff, serialized with atomic firewall rule replacement."""
from contextlib import contextmanager
import fcntl
import os
from pathlib import Path
import subprocess

STATE=Path('/var/lib/warden')
RULES='''table inet warden_cutoff {
 chain ingress {
  type filter hook prerouting priority -150; policy accept;
  iifname "lan0" tcp sport 2222 ct state established ct direction reply accept
  iifname "lan0" drop
 }
}
'''
@contextmanager
def locked():
    STATE.mkdir(parents=True,exist_ok=True)
    with (STATE/'network-control.lock').open('a') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX);yield

def apply_network(enabled):
    if type(enabled) is not bool:raise ValueError('invalid network state')
    with locked():
        flag=STATE/'network-disconnected'
        if not enabled:
            with flag.open('w') as stream:stream.write('disconnected\n');stream.flush();os.fsync(stream.fileno())
        exists=subprocess.run(['/usr/sbin/nft','list','table','inet','warden_cutoff'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL).returncode==0
        if enabled:
            if exists:subprocess.run(['/usr/sbin/nft','delete','table','inet','warden_cutoff'],check=True)
            flag.unlink(missing_ok=True)
        elif not exists:subprocess.run(['/usr/sbin/nft','-f','-'],input=RULES.encode(),check=True)

def load_firewall(path):
    with locked():
        rules=Path(path).read_text()
        if (STATE/'network-disconnected').exists():rules+='\n'+RULES
        # One nft transaction: replacement never opens a gap in the cutoff.
        subprocess.run(['/usr/sbin/nft','-f','-'],input=rules.encode(),check=True)
