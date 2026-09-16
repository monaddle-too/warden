import hashlib
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest

from warden.core import Audit, Engine, Redactor, dumps
from warden.retention import scrub_jsonl, scrub_requests, without_bodies


class RetentionTests(unittest.TestCase):
    def test_all_body_types_are_metadata_only(self):
        for body in [b'{"title":"private text"}', b'plain text', b'x=y', b'\x00\xff', b'']:
            self.assertEqual(Redactor().body(body), {'bytes':len(body),'capture':'omitted_policy'})

    def test_audit_rejects_body_content_from_an_old_proxy(self):
        with tempfile.TemporaryDirectory() as tmp:
            audit=Audit(Path(tmp)/'events.jsonl',Redactor())
            try:
                audit.emit('http.response',response={'body':{'bytes':42,'capture':'complete_redacted','content':'private payload','extra':'also private'}})
            finally: os.close(audit.fd)
            text=audit.path.read_text()
            self.assertNotIn('private',text)
            self.assertEqual(json.loads(text)['response']['body'],{'bytes':42,'capture':'omitted_policy'})

    def test_legacy_scrub_marks_and_rechains_without_backup(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp)/'events.jsonl'; events=[]; previous='0'*64
            for seq in [1,2]:
                event={'producer':{'instance_id':'a'},'sequence':seq,'previous_hash':previous,
                       'response':{'body':{'bytes':7,'content':'PRIVATE','capture':'complete_redacted'}}}
                event['event_hash']=hashlib.sha256(dumps(event).encode()).hexdigest()
                previous=event['event_hash']; events.append(event)
            path.write_text(''.join(dumps(e)+'\n' for e in events))
            self.assertEqual(scrub_jsonl(path),2)
            self.assertNotIn('PRIVATE',path.read_text())
            previous='0'*64
            for original,line in zip(events,path.read_text().splitlines()):
                event=json.loads(line); digest=event.pop('event_hash')
                self.assertEqual(event['previous_hash'],previous)
                self.assertEqual(event['retention_migration']['original_event_hash'],original['event_hash'])
                self.assertEqual(digest,hashlib.sha256(dumps(event).encode()).hexdigest())
                previous=digest
            self.assertEqual(scrub_jsonl(path),0)
            self.assertEqual(list(Path(tmp).iterdir()),[path])

    def test_approval_database_retains_metadata_and_user_constraints(self):
        with tempfile.TemporaryDirectory() as tmp:
            db=sqlite3.connect(Path(tmp)/'control.sqlite')
            db.execute('CREATE TABLE requests(id TEXT, summary TEXT)')
            db.execute('INSERT INTO requests VALUES(?,?)',('a',json.dumps({'body':{'bytes':7,'content':'PRIVATE'}})))
            db.commit()
            self.assertEqual(scrub_requests(db),1)
            self.assertNotIn('PRIVATE',db.execute('SELECT summary FROM requests').fetchone()[0])
            db.close()
            self.assertNotIn(b'PRIVATE',(Path(tmp)/'control.sqlite').read_bytes())
        self.assertEqual(without_bodies({'body_equals':{'/title':'approved title'}}),{'body_equals':{'/title':'approved title'}})
