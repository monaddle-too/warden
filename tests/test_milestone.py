import base64
import errno
import importlib.util
import json
import os
from pathlib import Path
import socket
import struct
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
import zipfile

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT/'host'));sys.path.insert(0,str(ROOT/'proxy'))
from warden import core,egress,environments,distribution
from warden.server import internal
import dns_guard

class PolicyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):cls.operations=core.Operations()
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.state=Path(self.temp.name)
        self.engine=core.Engine(self.state,self.operations)
    def tearDown(self):self.engine.db.close();os.close(self.engine.audit.fd);self.temp.cleanup()
    def request(self,host='registry.npmjs.org',method='GET',scheme='https',**extra):return {'host':host,'method':method,'scheme':scheme,**extra}
    def test_exact_destination_methods_and_tls_exceptions(self):
        self.assertTrue(self.engine.authorize_egress(self.request())['allow'])
        self.assertTrue(self.engine.authorize_egress(self.request('api.openai.com','POST'))['allow'])
        self.assertTrue(self.engine.authorize_egress(self.request('swscan.apple.com',tls=True))['allow'])
        for req in [self.request('registry.npmjs.org.evil.test'),self.request('source.registry.npmjs.org'),self.request(method='POST'),self.request(scheme='http'),self.request('example.com'),self.request('api.openai.com',tls=True)]:
            self.assertFalse(self.engine.authorize_egress(req)['allow'],req)
    def test_dns_never_resolves_unknown_names_in_restricted_mode(self):
        for host in ['registry.npmjs.org','github.com','api.github.com']:
            self.assertTrue(internal(self.engine,{'action':'dns','hostname':host})['allow'])
        for host in ['secret.example.com','secret.api.openai.com','evil.github.com','127.0.0.1']:
            self.assertFalse(internal(self.engine,{'action':'dns','hostname':host})['allow'])
    def test_disconnect_persists_and_revokes_inflight_access(self):
        decision=self.engine.authorize_egress(self.request())
        self.assertTrue(self.engine.active(decision['decision_id']))
        self.engine.set_network(False)
        self.assertFalse(self.engine.active(decision['decision_id']))
        self.assertFalse(internal(self.engine,{'action':'dns','hostname':'api.openai.com'})['allow'])
        restarted=core.Engine(self.state,self.operations)
        try:self.assertFalse(restarted.network_enabled)
        finally:restarted.db.close();os.close(restarted.audit.fd)
        self.engine.set_network(True)
        self.assertFalse(self.engine.active(decision['decision_id']))
    def test_policy_edit_invalidates_existing_network_decision(self):
        decision=self.engine.authorize_egress(self.request())
        self.engine.save_policy({**self.engine.policy,'egress':{'mode':'restricted','destinations':[]}})
        self.assertFalse(self.engine.active(decision['decision_id']))
    def test_project_repository_is_not_approvable_outside_allowlist(self):
        self.engine.save_policy({**self.engine.policy,'allowed_repositories':['acme/one']})
        request={'method':'GET','host':'api.github.com','path':'/repos/acme/two','headers':[]}
        self.assertEqual(self.engine.authorize(request)['status'],403)
        request['path']='/repos/acme/one';self.assertEqual(self.engine.authorize(request)['status'],428)
        request['path']='/user';self.assertEqual(self.engine.authorize(request)['status'],403)
    def test_disk_full_prevents_authorization(self):
        with patch.object(self.engine.audit,'emit',side_effect=OSError(errno.ENOSPC,'full')):
            with self.assertRaises(OSError):self.engine.authorize_egress(self.request())
        self.assertEqual(self.engine.network_decisions,{})
    def test_proxy_cannot_enable_network_or_revoke(self):
        for action in ['network','network.enable','revoke-all','policy']:
            with self.assertRaises(ValueError):internal(self.engine,{'action':action,'enabled':True})

class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.state=Path(self.temp.name)
        for path in environments.FILES:
            p=self.state/path;p.parent.mkdir(exist_ok=True,parents=True);p.write_bytes(('original '+path).encode())
    def tearDown(self):self.temp.cleanup()
    def test_restore_preserves_current_policy_and_audit(self):
        environments.capture(self.state,'clean')
        (self.state/'policy.json').write_text('current policy');(self.state/'audit').mkdir();(self.state/'audit/events.jsonl').write_text('current audit')
        (self.state/'mac/disk.raw').write_bytes(b'changed')
        environments.restore(self.state,'clean')
        self.assertEqual((self.state/'mac/disk.raw').read_bytes(),b'original mac/disk.raw')
        self.assertEqual((self.state/'policy.json').read_text(),'current policy')
        self.assertEqual((self.state/'audit/events.jsonl').read_text(),'current audit')
        self.assertTrue((self.state/'network-disconnected').exists())
    def test_corrupt_snapshot_never_replaces_live_disk(self):
        checkpoint=environments.capture(self.state,'clean')
        (checkpoint/'mac/disk.raw').write_bytes(b'corrupt')
        before=(self.state/'mac/disk.raw').read_bytes()
        with self.assertRaisesRegex(ValueError,'corrupt'):environments.restore(self.state,'clean')
        self.assertEqual((self.state/'mac/disk.raw').read_bytes(),before)
    def test_active_vm_cannot_be_captured(self):
        import fcntl
        with (self.state/'vm.lock').open('a') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX)
            with self.assertRaisesRegex(ValueError,'shut down'):environments.capture(self.state,'blocked')
    def test_interrupted_restore_rolls_back(self):
        environments.capture(self.state,'clean');(self.state/'mac/disk.raw').write_bytes(b'keep current')
        rename=Path.rename;count=0
        def fail(path,target):
            nonlocal count
            if '.restore-' in str(path) and '/new/' in str(path):
                count+=1
                if count==2:raise OSError('simulated interruption')
            return rename(path,target)
        with patch.object(Path,'rename',fail):
            with self.assertRaises(OSError):environments.restore(self.state,'clean')
        self.assertEqual((self.state/'mac/disk.raw').read_bytes(),b'keep current')
        self.assertFalse((self.state/'restore-journal.json').exists())
    def test_projects_never_copy_parent_credentials(self):
        (self.state/'admin-token').write_text('private')
        a=environments.project_create(self.state,'alpha',['acme/one']);b=environments.project_create(self.state,'beta',['acme/two'])
        self.assertFalse((a/'admin-token').exists());self.assertFalse((a/'mac').exists())
        self.assertNotEqual((a/'guest/client_key').read_bytes(),(b/'guest/client_key').read_bytes())
        self.assertEqual(json.loads((a/'policy.json').read_text())['allowed_repositories'],['acme/one'])

class DNSAndReleaseTests(unittest.TestCase):
    def packet(self):return struct.pack('!6H',123,256,1,0,0,0)+b'\x03api\x06openai\x03com\0'+struct.pack('!HH',1,1)
    def test_dns_rejects_opaque_and_compressed_queries(self):
        packet=self.packet();self.assertEqual(dns_guard.question(packet)[0],'api.openai.com')
        for bad in [packet[:12]+b'\xc0\x0c'+packet[-4:],packet[:-4]+struct.pack('!HH',16,1),b'bad']:
            with self.assertRaises((ValueError,IndexError,struct.error)):dns_guard.question(bad)
        with patch.object(dns_guard,'permitted',return_value=False),patch.object(dns_guard.socket,'socket') as connection:
            result=dns_guard.resolve(packet);self.assertEqual(result[3]&15,5);connection.assert_not_called()
    def test_signature_tamper_and_wrong_key_are_rejected(self):
        with tempfile.TemporaryDirectory() as temp:
            state=Path(temp);key=distribution.keygen(state);archive=state/'archive.zip';archive.write_bytes(b'fixture')
            manifest=state/'manifest.json';manifest.write_text(json.dumps({'version':1,'created':1,'sha256':environments.digest(archive),'bytes':archive.stat().st_size}))
            subprocess.run(['/usr/bin/ssh-keygen','-Y','sign','-f',str(key),'-n',distribution.NAMESPACE,str(manifest)],check=True,capture_output=True)
            signature=Path(str(manifest)+'.sig');public=Path(str(key)+'.pub')
            self.assertEqual(distribution.verify(archive,manifest,signature,public)['version'],1)
            archive.write_bytes(b'changed')
            with self.assertRaisesRegex(ValueError,'checksum'):distribution.verify(archive,manifest,signature,public)
            manifest.write_text('{}')
            with self.assertRaisesRegex(ValueError,'signature'):distribution.verify(archive,manifest,signature,public)
    def test_release_rejects_traversal_and_arbitrary_symlinks(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp)
            for i,path in enumerate(['warden/../../escape','/warden/escape','warden/host/link']):
                archive=root/(str(i)+'.zip')
                with zipfile.ZipFile(archive,'w') as z:
                    if path.endswith('/link'):
                        info=zipfile.ZipInfo(path);info.external_attr=0o120777<<16;z.writestr(info,'/etc')
                    else:z.writestr(path,b'bad')
                with self.assertRaises(ValueError):distribution.extract(archive,root/str(i))

if __name__=='__main__':unittest.main()
