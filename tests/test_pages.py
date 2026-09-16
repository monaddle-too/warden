import json
from pathlib import Path
import tempfile
import threading
import unittest
from types import SimpleNamespace
from unittest.mock import patch
import sqlite3
from warden.core import Redactor
from warden import pages

class PageTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.path=Path(self.tmp.name)/'audit.jsonl';self.path.touch()
        self.db=sqlite3.connect(':memory:')
        self.db.execute('CREATE TABLE requests(id TEXT PRIMARY KEY,status TEXT,created REAL,summary TEXT,operation TEXT,path TEXT,repository TEXT)')
        self.db.execute('CREATE INDEX requests_created_id ON requests(created DESC,id DESC)')
        self.engine=SimpleNamespace(audit=SimpleNamespace(path=self.path),redactor=Redactor(),db=self.db,lock=threading.RLock())
    def tearDown(self): self.db.close();self.tmp.cleanup()
    def event(self,i,kind='http.request.external'):
        return {'event_type':kind,'event_id':str(i),'time':'2026-09-10T00:00:00Z','request':{'host':'example.com','path':'/item/'+str(i)}}
    def append(self,event):
        with self.path.open('ab') as f:f.write(json.dumps(event).encode()+b'\n')
    def collect(self,params=None):
        params=dict(params or {});items=[];seen=set()
        for _ in range(200):
            result=pages.traffic_page(self.engine,params);self.assertLessEqual(len(result['items']),pages.PAGE_SIZE)
            self.assertLessEqual(result['scanned_bytes'],pages.SCAN_BYTES)
            items.extend(result['items']);cursor=result['next_cursor']
            if not cursor:return items
            self.assertNotIn(cursor,seen,'cursor must make progress');seen.add(cursor);params['cursor']=cursor
        self.fail('pagination did not finish')
    def test_every_traffic_row_once_across_chunk_boundaries(self):
        for i in range(140):self.append(self.event(i))
        with patch.object(pages,'SCAN_BYTES',1200):
            rows=self.collect()
        self.assertEqual([r['event_id'] for r in rows],[str(i) for i in reversed(range(140))])
    def test_fixed_snapshot_ignores_new_appends_until_latest(self):
        for i in range(60):self.append(self.event(i))
        first=pages.traffic_page(self.engine,{})
        self.append(self.event(60))
        rest=self.collect({'cursor':first['next_cursor']})
        self.assertEqual([r['event_id'] for r in first['items']+rest],[str(i) for i in reversed(range(60))])
        self.assertEqual(pages.traffic_page(self.engine,{})['items'][0]['event_id'],'60')
    def test_partial_and_oversized_rows_do_not_load_or_hide_other_rows(self):
        self.append(self.event(1))
        with self.path.open('ab') as f:
            f.write(b'{"body":"')
            for _ in range(20):f.write(b'x'*65536)
            f.write(b'"}\n')
        self.append(self.event(2))
        with self.path.open('ab') as f:f.write(b'{"unfinished":')
        rows=self.collect()
        self.assertEqual([r['event_id'] for r in rows],['2','1'])
    def test_filter_scans_bounded_windows_and_keeps_cursor(self):
        for i in range(100):self.append(self.event(i,'dns.query' if i==0 else 'http.response'))
        with patch.object(pages,'SCAN_BYTES',1200):
            first=pages.traffic_page(self.engine,{'type':'dns.query'})
            self.assertFalse(first['items']);self.assertIsNotNone(first['next_cursor'])
            rows=self.collect({'type':'dns.query'})
        self.assertEqual([r['event_id'] for r in rows],['0'])
    def test_replaced_or_rewritten_log_cursor_rejected(self):
        for i in range(60):self.append(self.event(i))
        cursor=pages.traffic_page(self.engine,{})['next_cursor']
        with self.path.open('r+b') as f:f.seek(-2,2);f.write(b'X')
        with self.assertRaises(pages.StaleCursor):pages.traffic_page(self.engine,{'cursor':cursor})
        self.path.unlink();self.path.touch()
        with self.assertRaises(pages.StaleCursor):pages.traffic_page(self.engine,{'cursor':cursor})
    def test_page_projection_scrubs_secrets_and_never_returns_bodies(self):
        e=self.event(1);e['request'].update({'path':'/path?token=secret','headers':[['Authorization','Bearer secret']], 'body':{'text':'private body','bytes':12}})
        self.append(e)
        result=pages.traffic_page(self.engine,{})
        encoded=json.dumps(result)
        self.assertNotIn('private body',encoded);self.assertNotIn('Bearer secret',encoded);self.assertNotIn('token=secret',encoded)
        self.assertEqual(result['items'][0]['request']['body_bytes'],12)
    def test_history_keyset_ties_filters_and_bounded_summaries(self):
        for i in range(80):
            summary=json.dumps({'method':'GET','host':'api.github.com','path':'/repos/owner/repo'+str(i)})
            self.db.execute('INSERT INTO requests VALUES(?,?,?,?,?,?,?)',(f'{i:036}', 'approved' if i%2 else 'pending',100,summary,'repos/get','/repos/owner/repo'+str(i),'owner/repo'+str(i)))
        params={'status':'approved'};ids=[]
        for _ in range(5):
            result=pages.history_page(self.engine,params);ids.extend(r['id'] for r in result['items'])
            self.assertLessEqual(len(result['items']),25)
            if not result['next_cursor']:break
            params['cursor']=result['next_cursor']
        self.assertEqual(ids,[f'{i:036}' for i in reversed(range(80)) if i%2])
        page=pages.history_page(self.engine,{'q':'repo79'})
        self.assertEqual(len(page['items']),1)
        with self.assertRaises(ValueError):pages.history_page(self.engine,{'cursor':params['cursor'],'status':'pending'})
        self.db.execute('UPDATE requests SET summary=? WHERE id=?', ('x'*1000000,f'{79:036}'))
        self.assertLess(len(json.dumps(pages.history_page(self.engine,{}))),200000)
    def test_parameters_and_cursor_validation(self):
        for query in ['q=a&q=b','path=/etc/passwd','q='+'a'*129]:
            with self.assertRaises(ValueError):pages.parameters(query,{'q','cursor','type'})
        for cursor in ['not-json','e30',pages.pack([1,2])]:
            with self.assertRaises(ValueError):pages.traffic_page(self.engine,{'cursor':cursor})
