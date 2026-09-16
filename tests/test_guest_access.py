import base64
import hashlib
import json
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT/'host'))
sys.path.insert(0, str(ROOT/'proxy'))
from warden import guest
import management

KEY = 'ssh-ed25519 '+base64.b64encode(b'\0\0\0\x0bssh-ed25519\0\0\0\x20'+bytes(range(32))).decode()


class GuestAccessTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='warden access ')
        self.addCleanup(self.temp.cleanup)
        self.state = Path(self.temp.name)
        (self.state/'mac').mkdir()
        (self.state/'mac/machine.bin').write_bytes(b'instance-one')
        (self.state/'guest').mkdir()

    def pair(self, key=KEY, fingerprint=None, *extra):
        with patch.object(guest, 'initialize', return_value=self.state/'guest'), patch.object(guest, 'connect', return_value=(Mock(), {'key':key})):
            return guest.main(['--state',str(self.state),'pair','--user','developer','--fingerprint',fingerprint or guest.key_fingerprint(key),*extra])

    def test_pair_requires_verified_key_and_explicit_replacement(self):
        with self.assertRaisesRegex(ValueError, 'does not match'):
            self.pair(fingerprint='SHA256:wrong')
        self.assertFalse((self.state/'guest/known_hosts').exists())
        self.pair()
        replacement='ssh-ed25519 '+base64.b64encode(b'\0\0\0\x0bssh-ed25519\0\0\0\x20'+b'x'*32).decode()
        with self.assertRaisesRegex(ValueError, 'pairing changed'):
            self.pair(replacement)
        self.assertIn(KEY,(self.state/'guest/known_hosts').read_text())
        self.pair(replacement,None,'--replace')
        self.assertIn(replacement,(self.state/'guest/known_hosts').read_text())

    def test_machine_replacement_requires_pairing(self):
        self.pair()
        (self.state/'mac/machine.bin').write_bytes(b'instance-two')
        with self.assertRaisesRegex(ValueError,'VM identity changed'):
            guest.ssh_options(self.state)

    def test_client_disables_ambient_credentials_and_forwarding(self):
        self.pair()
        options=guest.ssh_options(self.state)
        for option in ['StrictHostKeyChecking=yes','IdentityAgent=none','IdentitiesOnly=yes','ForwardAgent=no','ClearAllForwardings=yes','PermitLocalCommand=no','GlobalKnownHostsFile=/dev/null']:
            self.assertIn(option,options)
        proxy=next(v for v in options if v.startswith('ProxyCommand='))
        self.assertIn('guest-access.py',proxy)
        self.assertIn('transport',proxy)
        self.assertIn('UserKnownHostsFile='+json.dumps(str(self.state/'guest/known_hosts')),options)

    def test_public_key_validation(self):
        expected='SHA256:'+base64.b64encode(hashlib.sha256(base64.b64decode(KEY.split()[1])).digest()).decode().rstrip('=')
        self.assertEqual(guest.key_fingerprint(KEY),expected)
        for bad in ['ssh-rsa AAAA',KEY+' comment',KEY+'\n'+KEY,'ssh-ed25519 AAAA']:
            with self.assertRaises(ValueError):guest.key_fingerprint(bad)

    def test_key_initialization_is_private_and_idempotent(self):
        directory=guest.initialize(self.state)
        before=(directory/'client_key').read_bytes()
        guest.initialize(self.state)
        self.assertEqual((directory/'client_key').read_bytes(),before)
        self.assertEqual(directory.stat().st_mode & 0o777,0o700)
        self.assertEqual((directory/'client_key').stat().st_mode & 0o777,0o600)
        guest.key_fingerprint((directory/'client_key.pub').read_text())

    def test_transfer_batch_rejects_command_injection(self):
        for path in ['', 'a\n!touch /tmp/injected','a\rget secret','a\0b']:
            with self.assertRaises(ValueError):guest.sftp_quote(path)
        self.assertEqual(guest.sftp_quote('-R'),'"./-R"')
        self.assertEqual(guest.sftp_quote('a "b" [x]*?'),'"a \\"b\\" [x]*?"')

    def test_metadata_audit_does_not_contain_command(self):
        self.pair()
        with patch.object(guest.subprocess,'run',return_value=Mock(returncode=37)) as run:
            self.assertEqual(guest.main(['--state',str(self.state),'exec','--','printf','secret $(false)']),37)
        argv=run.call_args.args[0]
        self.assertEqual(argv[-1],"printf 'secret $(false)'")
        audit=(self.state/'guest/access.jsonl').read_text()
        self.assertNotIn('secret',audit)
        rows=[json.loads(line) for line in audit.splitlines()]
        self.assertEqual([r['event'] for r in rows],['started','completed'])
        self.assertEqual(rows[-1]['exit_code'],37)


class GuestRelayTests(unittest.TestCase):
    def test_lease_is_unique_current_and_fixed_subnet(self):
        with tempfile.TemporaryDirectory() as temp:
            leases=Path(temp)/'leases'
            good='2000 '+management.GUEST_MAC+' 10.77.0.73 guest *\n'
            leases.write_text(good)
            self.assertEqual(management.guest_address(leases,now=1000),'10.77.0.73')
            for invalid in [good+good,good.replace('2000','999'),good.replace('10.77.0.73','127.0.0.1'),good.replace('10.77.0.73','10.77.0.1'),good.replace(management.GUEST_MAC,'02:00:00:00:00:01')]:
                leases.write_text(invalid)
                with self.assertRaises(ValueError):management.guest_address(leases,now=1000)

    def request(self,message):
        client,server=socket.socketpair()
        client.settimeout(3)
        thread=threading.Thread(target=management.handle,args=(server,),daemon=True);thread.start()
        with client:
            client.sendall(json.dumps(message).encode()+b'\n')
            result=json.loads(client.makefile('rb').readline())
        thread.join(3)
        self.assertFalse(thread.is_alive())
        return result

    def test_keyscan_has_fixed_destination_and_port(self):
        output=Mock(stdout=('[10.77.0.73]:2222 '+KEY+'\n').encode())
        with patch.object(management,'guest_address',return_value='10.77.0.73'), patch.object(management.subprocess,'run',return_value=output) as run:
            self.assertEqual(self.request({'action':'guest.keyscan'}),{'key':KEY})
            self.assertEqual(run.call_args.args[0],['ssh-keyscan','-T','5','-p','2222','-t','ed25519','10.77.0.73'])

    def test_stream_does_not_accept_caller_destination(self):
        with patch.object(management.socket,'create_connection') as connect:
            self.assertIn('error',self.request({'action':'guest.stream','host':'example.com','port':443}))
            connect.assert_not_called()

    def test_stream_handshake_preserves_binary_bytes(self):
        client,server=socket.socketpair();upstream,remote=socket.socketpair()
        for conn in [client,remote]:conn.settimeout(3)
        with patch.object(management,'guest_address',return_value='10.77.0.73'), patch.object(management.socket,'create_connection',return_value=upstream) as connect:
            thread=threading.Thread(target=management.handle,args=(server,),daemon=True);thread.start()
            with client,remote:
                # Coalesce handshake and SSH bytes to catch buffered read-ahead.
                client.sendall(b'{"action":"guest.stream"}\nSSH-2.0-test\r\n')
                response=bytearray()
                while not response.endswith(b'\n'):response.extend(client.recv(1))
                self.assertEqual(json.loads(response),{'ready':True})
                self.assertEqual(remote.recv(100),b'SSH-2.0-test\r\n')
                remote.sendall(bytes(range(256)))
                self.assertEqual(client.recv(256),bytes(range(256)))
            thread.join(3)
            self.assertFalse(thread.is_alive())
            connect.assert_called_once_with(('10.77.0.73',2222),timeout=10)


if __name__=='__main__':unittest.main()
