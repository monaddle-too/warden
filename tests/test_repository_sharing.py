import os
import tempfile
import unittest
from unittest.mock import Mock
from pathlib import Path
from warden.sharing import Sharing
from warden.core import Engine

class RepositorySharingTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.github = Mock(owner='owner', app_id=123)
        self.github.repositories.return_value = {'repositories':[{'id':5,'full_name':'owner/repo','private':True}], 'next_page':None}
        self.github.authorization.return_value = 'Bearer synthetic-repository-secret'
        self.s = Sharing(self.tmp.name, github=self.github)
        self.e = Engine(Path(self.tmp.name)/'engine')
        self.context = {'chatID':'chat','sandboxID':'sandbox'}
    def tearDown(self):
        self.s.db.close(); self.e.db.close(); os.close(self.e.audit.fd); self.tmp.cleanup()
    def select(self, repos=None):
        return self.s.dispatch('github_select', {**self.context,'repositories':repos if repos is not None else ['owner/repo']})
    def read(self, **kwargs):
        return self.s.github_grant('chat','sandbox',self.e, {'host':'api.github.com','method':'GET','path':'/repos/owner/repo','headers':[],**kwargs})
    def test_persistence_scope_removal_and_identity(self):
        self.assertIsNone(self.read()); self.select()
        grant, auth = self.read(); self.assertEqual(auth,'Bearer synthetic-repository-secret')
        self.github.authorization.assert_called_with('owner/repo','repos/get',repository_id=5)
        self.s.db.close(); self.s=Sharing(self.tmp.name,github=self.github)
        self.assertTrue(self.s.github_active(grant,'chat','sandbox'))
        # Repositories are shared with the environment, so a sibling chat sees them too.
        self.assertTrue(self.s.github_active(grant,'other','sandbox'))
        self.assertEqual(self.s.github_grant('other','sandbox',self.e,{'host':'api.github.com','method':'GET','path':'/repos/owner/repo','headers':[]})[0],grant)
        self.assertEqual(self.s.dispatch('github_list',{'chatID':'other','sandboxID':'sandbox'})['repositories'][0]['full_name'],'owner/repo')
        self.assertFalse(self.s.github_active(grant,'chat','other'))
        self.assertEqual(self.s.dispatch('github_list',self.context)['repositories'][0]['expires_at'],None)
        self.github.owner='other'; self.assertFalse(self.s.github_active(grant,'chat','sandbox')); self.github.owner='owner'
        self.select([]); self.assertIsNone(self.read()); self.assertFalse(self.s.github_active(grant,'chat','sandbox'))
    def test_no_writes_or_other_repositories(self):
        self.select()
        for request in ({'method':'POST','path':'/repos/owner/repo/git/blobs'}, {'path':'/repos/owner/other'}, {'host':'raw.githubusercontent.com'}, {'scheme':'http'}, {'body_base64':'e30=','headers':[['content-type','application/json']]}):
            self.assertIsNone(self.read(**request))
        self.assertEqual(self.github.authorization.call_count,0)
        self.e.network_enabled=False; self.assertIsNone(self.read())
    def test_git_read_and_route_ambiguity(self):
        self.select()
        self.assertIsNotNone(self.read(host='github.com',path='/owner/repo.git/info/refs?service=git-upload-pack'))
        for path in ('/repos/owner/repo/../other','/repos/owner%2Frepo','//api.github.com/repos/owner/repo'):
            with self.assertRaises(ValueError): self.read(path=path)
    def test_invalid_selection_is_atomic(self):
        self.select()
        for names in (['owner/missing'],['else/repo'],['owner/repo','OWNER/REPO']):
            with self.assertRaises(ValueError):self.select(names)
        self.assertEqual(len(self.s.dispatch('github_list',self.context)['repositories']),1)
        self.github.repositories.side_effect=OSError('offline')
        with self.assertRaises(OSError):self.select()
        self.select([]) # Revocation works even when GitHub is offline.
        self.assertEqual(self.s.dispatch('github_list',self.context)['repositories'],[])
