import base64
import copy
import tempfile
import unittest
from unittest.mock import Mock, patch
from warden.sharing import Sharing
from warden.pull_requests import relevant_entries

A='a'*40;B='b'*40;C='c'*40;D='d'*40;E='e'*40

class TreeTraversalTests(unittest.TestCase):
    def test_only_changed_directories_are_loaded_and_ancestors_preserved(self):
        calls=[]
        trees={A:[{'path':'docs','type':'tree','sha':B}, {'path':'huge-unrelated','type':'tree','sha':E},
                  {'path':'link','type':'blob','mode':'120000','sha':E}],
               B:[{'path':'page.md','type':'blob','mode':'100644','sha':C}]}
        def call(method,path,op):
            calls.append(path)
            return {'tree':trees[path.rsplit('/',1)[1]], 'truncated':False}
        entries=relevant_entries(call,A,{'docs/page.md','docs/new.md','link/secret','missing/new'})
        self.assertEqual(calls,['/git/trees/'+A,'/git/trees/'+B])
        self.assertEqual(entries['link']['mode'],'120000')
        self.assertEqual(entries['docs/page.md']['sha'],C)
        self.assertNotIn('docs/new.md',entries)

    def test_truncated_or_malformed_directory_fails_closed(self):
        for listing in ({'truncated':True,'tree':[]}, {'tree':[{'path':'../hidden'}]},
                        {'tree':[{'path':'same'},{'path':'same'}]}):
            with self.assertRaises(ValueError):
                relevant_entries(lambda *args:listing,A,{'file'})

class PullRequestTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.github=Mock(owner='owner',app_id=123)
        self.github.repositories.return_value={'repositories':[{'id':7,'full_name':'owner/repo'}],'next_page':None}
        self.github.authorization.return_value='Bearer synthetic-app-credential'
        self.s=Sharing(self.tmp.name,github=self.github);self.calls=[]
        self.s.dispatch('github_select',{'chatID':'chat','sandboxID':'sandbox','repositories':['owner/repo']})
        self.s.pull_requests.transport=self.transport
        self.data={'chatID':'chat','sandboxID':'sandbox','callID':'call','repository':'owner/repo','base':'main','title':'Improve greeting','body':'A clear description.','files':[{'path':'hello.txt','content':'hello world\n'}]}
    def tearDown(self):self.s.db.close();self.tmp.cleanup()
    def transport(self, method,path,token,body=None):
        self.calls.append((method,path,copy.deepcopy(body)))
        if '/git/ref/heads/' in path:return {'object':{'sha':A}}
        if method=='GET' and '/git/commits/' in path:return {'tree':{'sha':B}}
        if method=='GET' and '/git/trees/' in path:return {'tree':[{'path':'hello.txt','sha':C,'type':'blob','mode':'100644','size':6}],'truncated':False}
        if method=='GET' and '/git/blobs/' in path:return {'encoding':'base64','content':base64.b64encode(b'hello\n').decode()}
        if path.endswith('/git/trees'):return {'sha':D}
        if path.endswith('/git/commits'):return {'sha':E}
        if path.endswith('/git/refs'):return {'ref':body['ref']}
        if path.endswith('/pulls'):return {'number':42}
        raise AssertionError(path)
    def submit(self):return self.s.dispatch('pr_submit',self.data)
    def resolve(self,id,allow,feedback='',body=None):
        if body is None:
            import json
            body=json.loads(self.s.pull_requests.row(id)['proposal'])['body']
        with patch('warden.pull_requests.threading.Thread'):
            return self.s.dispatch('pr_resolve',{'id':id,'allow':allow,'feedback':feedback,'body':body})
    def test_preview_computed_by_host_and_no_writes_before_approval(self):
        r=self.submit();p=self.s.dispatch('pr_preview',{'id':r['request_id']})['proposal']
        self.assertEqual(p['base_sha'],A);self.assertIn('-hello',p['files'][0]['diff']);self.assertIn('+hello world',p['files'][0]['diff'])
        self.assertTrue(all(c[0]=='GET' for c in self.calls))
        self.assertEqual(self.submit()['request_id'],r['request_id'])
        with self.assertRaises(ValueError):self.s.dispatch('pr_get',{'id':r['request_id'],'chatID':'other','sandboxID':'sandbox'})
    def test_rejection_feedback_durable_and_no_write(self):
        r=self.submit();self.resolve(r['request_id'],False,'Use a shorter greeting')
        self.s.db.close();self.s=Sharing(self.tmp.name,github=self.github)
        delivery=self.s.dispatch('undelivered',{})['requests'][0]
        self.assertEqual(delivery['feedback'],'Use a shorter greeting');self.assertEqual(delivery['status'],'rejected')
        self.resolve(r['request_id'],True);self.assertEqual(self.s.pull_requests.row(r['request_id'])['status'],'rejected')
        self.s.dispatch('ack',{'id':r['request_id']});self.assertEqual(self.s.dispatch('undelivered',{})['requests'],[])
        self.assertTrue(all(c[0]=='GET' for c in self.calls))
    def test_approved_snapshot_publishes_once(self):
        r=self.submit();id=r['request_id'];self.data['files'][0]['content']='unreviewed mutation'
        self.resolve(id,True);self.s.pull_requests.publish(id)
        out=self.s.dispatch('pr_get',{'id':id,'chatID':'chat','sandboxID':'sandbox'})
        self.assertEqual(out['url'],'https://github.com/owner/repo/pull/42')
        writes=[c for c in self.calls if c[0]=='POST'];self.assertEqual(len(writes),4)
        self.assertEqual(writes[0][2]['tree'][0]['content'],'hello world\n')
        self.assertEqual(writes[1][2]['parents'],[A]);self.assertEqual(writes[-1][2]['body'],'A clear description.')
        self.assertEqual(writes[-1][2]['head'],'warden/pr-'+id[:24])
        self.resolve(id,True);self.s.pull_requests.publish(id);self.assertEqual(len([c for c in self.calls if c[0]=='POST']),4)
    def test_changed_base_and_revoked_repository_block_writes(self):
        for mode in ('base','revoke'):
            self.data['callID']=mode;id=self.submit()['request_id'];self.resolve(id,True)
            if mode=='base':
                transport=self.s.pull_requests.transport
                self.s.pull_requests.transport=lambda *args: {'object':{'sha':E}}
            else:self.s.dispatch('github_select',{'chatID':'chat','sandboxID':'sandbox','repositories':[]})
            self.s.pull_requests.publish(id)
            self.assertEqual(self.s.pull_requests.row(id)['status'],'failed')
            self.assertFalse(any(c[0]=='POST' for c in self.calls))
            if mode=='base':self.s.pull_requests.transport=transport
    def test_ambiguous_failure_and_restart_never_republish(self):
        id=self.submit()['request_id'];self.resolve(id,True)
        original=self.transport
        def fail(*args):
            if args[1].endswith('/pulls'):raise OSError('sensitive upstream diagnostic')
            return original(*args)
        self.s.pull_requests.transport=fail;self.s.pull_requests.publish(id)
        out=self.s.dispatch('pr_get',{'id':id,'chatID':'chat','sandboxID':'sandbox'})
        self.assertEqual(out['status'],'failed');self.assertNotIn('sensitive',str(out));self.assertIn('branch_url',out)
        writes=len(self.calls);self.resolve(id,True);self.s.pull_requests.publish(id);self.assertEqual(len(self.calls),writes)
        self.data['callID']='interrupted';self.s.pull_requests.transport=self.transport
        id=self.submit()['request_id'];self.resolve(id,True);self.s.db.close();self.s=Sharing(self.tmp.name,github=self.github)
        self.assertEqual(self.s.pull_requests.row(id)['status'],'failed')
    def test_input_boundaries_and_unselected_repositories(self):
        original=copy.deepcopy(self.data)
        for changed in ({'repository':'other/repo'},{'base':'../main'},{'files':[{'path':'../secret','content':'x'}]}, {'files':[{'path':'.github/workflows/run.yml','content':'x'}]}, {'files':[{'path':'a','content':'\0'}]}, {'files':[{'path':'a','content':'x'*262145}]}, {'files':[{'path':'a','content':'x'},{'path':'a/b','content':'y'}]}):
            self.data={**original,**changed}
            self.assertEqual(self.submit()["status"],"invalid")
        self.assertFalse(any(c[0]=='POST' for c in self.calls))
    def test_deletion_addition_and_diff_marker_content(self):
        self.data['files']=[{'path':'hello.txt','content':None},{'path':'new.txt','content':'++literal\n--literal'}]
        id=self.submit()['request_id'];p=self.s.dispatch('pr_preview',{'id':id})['proposal']
        self.assertEqual(p['files'][1]['additions'],2)
        self.assertIn('\\ No newline at end of file',p['files'][1]['diff'])
        self.resolve(id,True);self.s.pull_requests.publish(id)
        tree=next(c[2] for c in self.calls if c[0]=='POST' and c[1].endswith('/git/trees'))
        self.assertIsNone(tree['tree'][0]['sha'])

    def test_exact_edited_body_is_the_published_body(self):
        id=self.submit()['request_id']
        edited="## Reviewed by owner\n\nKeep **this** wording — exactly.\n"
        self.resolve(id,True,body=edited)
        self.assertEqual(self.s.dispatch('pr_preview',{'id':id})['proposal']['body'],edited)
        self.s.pull_requests.publish(id)
        outgoing=next(c[2] for c in self.calls if c[1].endswith('/pulls'))
        self.assertEqual(outgoing['body'].encode('utf-8'),edited.encode('utf-8'))

    def test_body_mismatch_and_missing_review_fail_closed(self):
        import json
        from warden.pull_requests import reviewed_body
        id=self.submit()['request_id']
        with self.assertRaises(ValueError):self.s.dispatch('pr_resolve',{'id':id,'allow':True})
        self.assertEqual(self.s.pull_requests.row(id)['status'],'pending')
        self.resolve(id,True,body='Approved body\n')
        with self.assertRaises(ValueError): self.resolve(id,True,body='Unreviewed retry')
        p=json.loads(self.s.pull_requests.row(id)['proposal'])
        for candidate in ('Changed body\n','Approved body','Approved body\r\n'):
            with self.assertRaises(ValueError):reviewed_body(p,candidate)
        p['body']='Changed after approval'
        with self.s.db:self.s.db.execute('UPDATE pull_requests SET proposal=? WHERE id=?',(json.dumps(p),id))
        self.s.pull_requests.publish(id)
        self.assertEqual(self.s.pull_requests.row(id)['status'],'failed')
        self.assertFalse(any(c[0]=='POST' for c in self.calls))

    def test_rejection_returns_owner_body_edits_for_revision(self):
        id=self.submit()['request_id'];self.resolve(id,False,'Revise the code too',body='Suggested replacement body')
        result=self.s.dispatch('pr_get',{'id':id,'chatID':'chat','sandboxID':'sandbox'})
        self.assertEqual(result['edited_body'],'Suggested replacement body')
        self.assertEqual(result['feedback'],'Revise the code too')
        self.assertFalse(any(c[0]=='POST' for c in self.calls))

    def test_review_never_grants_agent_permission_to_post_a_different_body(self):
        import json,os
        from warden.core import Engine
        engine=Engine(self.tmp.name+'/engine')
        try:
            id=self.submit()['request_id'];self.resolve(id,True,body='Owner approved body')
            req={'host':'api.github.com','scheme':'https','port':443,'method':'POST','path':'/repos/owner/repo/pulls',
                'headers':[['content-type','application/json']], 'body_base64':base64.b64encode(json.dumps({'title':'Improve greeting','head':'branch','base':'main','body':'Unreviewed agent body'}).encode()).decode()}
            self.assertIsNone(self.s.github_grant('chat','sandbox',engine,req))
            decision=engine.authorize(req)
            self.assertFalse(decision['allow']);self.assertNotIn('authorization',decision)
        finally:engine.db.close();os.close(engine.audit.fd)

    def test_images_are_reviewed_and_published_without_rewriting_body(self):
        from test_images import PNG
        image=self.s.dispatch('image_add',{'chatID':'chat','sandboxID':'sandbox','caption':'Screenshot','png':base64.b64encode(PNG).decode()})
        self.data['images']=[image['image_id']];id=self.submit()['request_id'];p=self.s.dispatch('pr_preview',{'id':id})['proposal']
        self.assertIn('../blob/'+p['head']+'/.warden/images/',p['body']);self.assertEqual(p['images'][0]['image_id'],image['image_id'])
        original=self.transport
        def transport(method,path,token,body=None):
            if method=='POST' and path.endswith('/git/blobs'):
                self.calls.append((method,path,copy.deepcopy(body)));return {'sha':C}
            return original(method,path,token,body)
        self.s.pull_requests.transport=transport;self.resolve(id,True);self.s.pull_requests.publish(id)
        payload=next(c[2] for c in self.calls if c[1].endswith('/pulls'));self.assertEqual(payload['body'],p['body'])
        blob=next(c[2] for c in self.calls if c[0]=='POST' and c[1].endswith('/git/blobs'));self.assertEqual(base64.b64decode(blob['content']),PNG)
        tree=next(c[2] for c in self.calls if c[0]=='POST' and c[1].endswith('/git/trees'));self.assertEqual(tree['tree'][-1]['sha'],C)

    def test_foreign_image_cannot_be_attached_to_proposal(self):
        from test_images import PNG
        image=self.s.dispatch('image_add',{'chatID':'other','sandboxID':'sandbox','caption':'Private','png':base64.b64encode(PNG).decode()})
        self.data['images']=[image['image_id']];self.assertEqual(self.submit()['status'],'invalid')
        self.assertFalse(any(c[0]=='POST' for c in self.calls))
