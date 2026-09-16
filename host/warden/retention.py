"""Enforce metadata-only HTTP audit retention, including legacy records."""
from __future__ import annotations
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import tempfile


def without_bodies(value):
    if isinstance(value, list):
        return [without_bodies(item) for item in value]
    if not isinstance(value, dict):
        return value
    result = {}
    for key, item in value.items():
        if key == 'body':
            body = {'capture': 'omitted_policy'}
            if isinstance(item, dict) and type(item.get('bytes')) is int and item['bytes'] >= 0:
                body['bytes'] = item['bytes']
            result[key] = body
        elif key == 'body_base64':
            result[key] = '[OMITTED]'
        else:
            result[key] = without_bodies(item)
    return result


def encode(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=True, allow_nan=False)


def scrub_jsonl(path):
    """Rewrite only explicit audit files. Caller must stop their writers first.

    Hashes change when evidence is removed. Mark the migration and retain the
    original hashes as references, then compute a new chain for each instance.
    No body-bearing backup is made.
    """
    path = Path(path)
    if not path.exists():
        return 0
    with path.open() as stream:
        changed = sum(without_bodies(event) != event for event in map(json.loads, stream))
    if not changed:
        return 0
    previous = {}
    fd, tmp = tempfile.mkstemp(prefix=path.name+'.metadata-', dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as out, path.open() as source:
            for line in source:
                original = json.loads(line)
                event = without_bodies(original)
                # ECS exports wrap the native, hash-chained event in `warden`.
                native = event.get('warden', event)
                if isinstance(native, dict) and 'event_hash' in native and 'producer' in native:
                    instance = native['producer']['instance_id']
                    native['retention_migration'] = {
                        'operation': 'remove_http_bodies',
                        'original_event_hash': native.pop('event_hash'),
                        'original_previous_hash': native['previous_hash'],
                    }
                    native['previous_hash'] = previous.get(instance, '0'*64)
                    native['event_hash'] = hashlib.sha256(encode(native).encode()).hexdigest()
                    previous[instance] = native['event_hash']
                out.write(encode(event)+'\n')
            out.flush(); os.fsync(out.fileno())
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp): os.unlink(tmp)
    return changed


def scrub_requests(db):
    """Remove captured bodies from approval summaries and SQLite free pages."""
    db.execute('PRAGMA secure_delete=ON')
    changed = 0
    for row in db.execute('SELECT id,summary FROM requests'):
        summary = json.loads(row[1]); cleaned = without_bodies(summary)
        if cleaned != summary:
            db.execute('UPDATE requests SET summary=? WHERE id=?', (encode(cleaned), row[0]))
            changed += 1
    db.commit()
    if changed:
        db.execute('PRAGMA wal_checkpoint(TRUNCATE)')
        db.execute('VACUUM')
        db.execute('PRAGMA wal_checkpoint(TRUNCATE)')
    return changed
