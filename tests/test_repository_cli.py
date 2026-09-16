import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT=Path(__file__).resolve().parents[1]

@unittest.skipUnless(shutil.which('node') and shutil.which('npm') and shutil.which('git'),'Node, npm and Git required')
class RepositoryCLITests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name)
        self.env={**os.environ,'GIT_CONFIG_NOSYSTEM':'1','GIT_CONFIG_GLOBAL':'/dev/null',
            'GIT_AUTHOR_NAME':'Test','GIT_AUTHOR_EMAIL':'test@example.invalid','GIT_COMMITTER_NAME':'Test','GIT_COMMITTER_EMAIL':'test@example.invalid'}
        self.run_cmd('git','init','-b','main')
        self.run_cmd('git','remote','add','origin','https://github.com/acme/demo.git')
    def tearDown(self):self.tmp.cleanup()
    def run_cmd(self,*argv,ok=True):
        result=subprocess.run(argv,cwd=self.root,env=self.env,capture_output=True,text=True,timeout=30)
        if ok:self.assertEqual(result.returncode,0,result.stderr)
        return result
    def cli(self,*argv,ok=True):return self.run_cmd(shutil.which('node'),str(ROOT/'scripts/warden-repo'),*argv,ok=ok)
    def commit(self):self.run_cmd('git','add','.');self.run_cmd('git','commit','-m','Fixture')
    def test_checks_receipts_dirty_tree_and_changed_commit(self):
        (self.root/'.warden').mkdir()
        (self.root/'.warden/workflow.json').write_text(json.dumps({'version':1,'setup':'none','checks':[['node','-e','if (2+2!==4) process.exit(1)']]}))
        self.commit();self.cli('check')
        receipt=json.loads((self.root/'.git/warden-check.json').read_text())
        self.assertEqual(receipt['head'],self.run_cmd('git','rev-parse','HEAD').stdout.strip())
        (self.root/'change').write_text('new')
        self.assertNotEqual(self.cli('check',ok=False).returncode,0)
        self.commit()
        result=self.cli('publish','--wait','0',ok=False)
        self.assertNotEqual(result.returncode,0);self.assertIn('different commit',result.stderr)
    def test_failed_checks_do_not_leave_a_receipt(self):
        (self.root/'.warden').mkdir()
        (self.root/'.warden/workflow.json').write_text(json.dumps({'version':1,'setup':'none','checks':[['node','-e','process.exit(7)']]}))
        self.commit();self.assertNotEqual(self.cli('check',ok=False).returncode,0)
        self.assertFalse((self.root/'.git/warden-check.json').exists())
    def test_npm_setup_does_not_execute_lifecycle_scripts(self):
        (self.root/'package.json').write_text(json.dumps({'name':'fixture','version':'1.0.0','scripts':{'postinstall':"node -e \"require('fs').writeFileSync('ran-script','yes')\""}}))
        (self.root/'package-lock.json').write_text(json.dumps({'name':'fixture','version':'1.0.0','lockfileVersion':3,'packages':{'':{'name':'fixture','version':'1.0.0','hasInstallScript':True}}}))
        self.cli('setup');self.assertFalse((self.root/'ran-script').exists())
        self.cli('setup','--scripts');self.assertTrue((self.root/'ran-script').exists())
    def test_remote_credentials_and_existing_clone_destination_rejected(self):
        self.run_cmd('git','remote','set-url','origin','https://user:password@github.com/acme/demo.git')
        self.assertNotEqual(self.cli('status',ok=False).returncode,0)
        (self.root/'occupied').mkdir();(self.root/'occupied/keep').write_text('preserved')
        self.assertNotEqual(self.cli('clone','acme/demo','occupied',ok=False).returncode,0)
        self.assertEqual((self.root/'occupied/keep').read_text(),'preserved')
