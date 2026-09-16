"""Durable host-side OCSF delivery; independent of the authorization path."""
import argparse
from collections import OrderedDict
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path
import random
import re
import signal
import sqlite3
import stat
import threading
import time
from urllib.parse import urlsplit

from .ocsf import Exporter

MAX_RECORDS = 500
MAX_BODY = 1024 * 1024
MAX_EVENT = 256 * 1024
MAX_NATIVE = 12 * 1024 * 1024


class DeliveryError(Exception):
    """Only fixed, credential-free messages may be displayed to operators."""


def encode(value):
    return json.dumps(value, separators=(',', ':'), ensure_ascii=False, allow_nan=False)


def error_message(exc):
    return str(exc) if isinstance(exc, DeliveryError) else 'SIEM operation failed (' + type(exc).__name__ + '); local events retained'


def configuration(state):
    value = json.loads((state / 'siem.json').read_text())
    url = urlsplit(value['endpoint'])
    if (url.scheme != 'https' or not url.hostname or url.username or url.password
            or url.query or url.fragment or url.path != '/api/v1/events'):
        raise DeliveryError('SIEM endpoint must be an HTTPS /api/v1/events URL')
    source = value.get('source', 'warden')
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,63}', source):
        raise DeliveryError('Invalid SIEM source')
    token_path = Path(value.get('token_file', 'siem-token'))
    if not token_path.is_absolute(): token_path = state / token_path
    return value['endpoint'], source, token_path


def read_token(path):
    with path.open() as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.getuid():
            raise DeliveryError('SIEM token must be an owner-only file (chmod 600)')
        token = stream.read(4097).strip()
    if not re.fullmatch(r'[A-Za-z0-9._~+/=-]{32,4096}', token):
        raise DeliveryError('Invalid SIEM token file')
    return token


def post(endpoint, source, token, key, body):
    # Direct TLS, no ambient HTTP proxy, redirects, cookies, or token-bearing logs.
    url = urlsplit(endpoint)
    conn = http.client.HTTPSConnection(url.hostname, url.port or 443, timeout=20)
    try:
        conn.request('POST', url.path, body=body, headers={
            'Authorization': 'Bearer ' + token,
            'Content-Type': 'application/x-ndjson',
            'X-OCSF-Source': source, 'Idempotency-Key': key})
        response = conn.getresponse()
        if response.status != 202:
            raise DeliveryError('SIEM HTTP ' + str(response.status) + '; pending batch retained')
        raw = response.read(65537)
        if len(raw) > 65536: raise DeliveryError('Oversized SIEM receipt')
        try: return json.loads(raw)
        except (ValueError, UnicodeError): raise DeliveryError('Invalid SIEM receipt') from None
    finally:
        conn.close()


def anchor(stream, offset):
    stream.seek(max(0, offset - 256))
    return hashlib.sha256(stream.read(min(offset, 256))).hexdigest()


class Shipper:
    def __init__(self, state, transport=post):
        self.state = Path(state)
        self.endpoint, self.source, self.token_path = configuration(self.state)
        self.transport = transport
        self.audit = self.state / 'audit/events.jsonl'
        self.db = sqlite3.connect(self.state / 'siem.sqlite')
        self.db.execute('PRAGMA synchronous=FULL')
        self.db.execute('CREATE TABLE IF NOT EXISTS state (id INTEGER PRIMARY KEY CHECK(id=1), data TEXT NOT NULL, body BLOB)')
        row = self.db.execute('SELECT data FROM state WHERE id=1').fetchone()
        if row:
            data = json.loads(row[0])
            if data['endpoint'] != self.endpoint or data['source'] != self.source:
                self.db.close()
                raise DeliveryError('SIEM destination changed; reconcile the existing cursor before switching')
        else:
            self.save({'endpoint': self.endpoint, 'source': self.source, 'offset': 0,
                       'identity': None, 'anchor': None, 'context': [],
                       'delivered_events': 0, 'delivered_batches': 0})

    def close(self):
        self.db.close()

    def load(self):
        data, body = self.db.execute('SELECT data,body FROM state WHERE id=1').fetchone()
        return json.loads(data), body

    def save(self, data, body=None):
        with self.db:
            self.db.execute('INSERT OR REPLACE INTO state VALUES (1,?,?)', (encode(data), body))

    def prepare(self, data):
        exporter = Exporter()
        exporter.requests = OrderedDict((tuple(key), value) for key, value in data['context'])
        records, size = [], 0
        with self.audit.open('rb') as stream:
            info = os.fstat(stream.fileno())
            identity = [info.st_dev, info.st_ino]
            if data['identity'] is not None and (identity != data['identity']
                    or info.st_size < data['offset'] or anchor(stream, data['offset']) != data['anchor']):
                raise DeliveryError('Audit file replaced, rewritten, or truncated; cursor retained for reconciliation')
            stream.seek(data['offset'])
            while len(records) < MAX_RECORDS:
                position = stream.tell()
                line = stream.readline(MAX_NATIVE + 1)
                if len(line) > MAX_NATIVE:
                    if records: break
                    raise DeliveryError('Audit record exceeds local size limit; cursor retained')
                if not line or not line.endswith(b'\n'): break
                try:
                    # Convert only a record that will fit. Undo cache mutation if
                    # this record must be left for the next batch.
                    context = exporter.requests.copy()
                    record = encode(exporter.convert(json.loads(line))).encode('utf-8') + b'\n'
                    if len(record) - 1 > MAX_EVENT:
                        raise ValueError('oversized event')
                except (ValueError, TypeError, KeyError, AttributeError, RecursionError):
                    if records:
                        exporter.requests = context
                        break
                    raise DeliveryError('Invalid or oversized OCSF record at byte ' + str(position) + '; cursor retained') from None
                if size + len(record) > MAX_BODY:
                    exporter.requests = context
                    break
                records.append(record)
                size += len(record)
                end = stream.tell()
            if not records: return None
            pending = {'end': end, 'identity': identity, 'anchor': anchor(stream, end),
                       'context': list(exporter.requests.items()), 'count': len(records),
                       'created': time.time()}
        body = b''.join(records)
        pending['key'] = 'warden-' + hashlib.sha256(body).hexdigest()
        data['pending'] = pending
        # Bytes, key, correlation cache, and proposed cursor commit together
        # BEFORE any network I/O. Restart never re-batches an uncertain request.
        self.save(data, body)
        return body

    def step(self):
        data, body = self.load()
        if body is None:
            body = self.prepare(data)
            if body is None: return False
        pending = data['pending']
        # Keep retrying after long outages. The server expires deduplication
        # receipts after seven days, so delivery is at least once across that
        # boundary; preserving events takes priority over avoiding duplicates.
        receipt = self.transport(self.endpoint, self.source, read_token(self.token_path), pending['key'], body)
        if (not isinstance(receipt, dict) or not isinstance(receipt.get('id'), str)
                or not re.fullmatch(r'[a-f0-9]{32}', receipt['id'])
                or type(receipt.get('accepted')) is not int or receipt['accepted'] != pending['count']
                or type(receipt.get('rejected')) is not int or receipt['rejected'] != 0
                or receipt.get('state') not in ('queued', 'indexed')
                or receipt.get('problems') not in (None, [])):
            raise DeliveryError('SIEM receipt reports rejection or incomplete acceptance; pending batch retained')
        data.update(offset=pending['end'], identity=pending['identity'], anchor=pending['anchor'],
                    context=pending['context'], delivered_events=data['delivered_events'] + pending['count'],
                    delivered_batches=data['delivered_batches'] + 1, last_receipt=receipt['id'],
                    last_delivered=time.time())
        del data['pending']
        self.save(data)
        return True

    def retry_wait(self):
        data, _ = self.load()
        return max(0, (data.get('next_retry_at') or 0) - time.time())

    def poll(self):
        """One scheduled attempt; both pending data and retry timing survive exit."""
        if self.retry_wait() > 0: return False
        try:
            sent = self.step()
        except Exception as exc:
            # Load again: step may have persisted a new batch before failing.
            data, body = self.load()
            failures = data.get('consecutive_failures', 0) + 1
            now = time.time()
            delay = min(60, 2 ** min(failures, 6) + random.random())
            data.update(consecutive_failures=failures, last_attempt_at=now,
                        next_retry_at=now + delay, last_error=error_message(exc))
            self.save(data, body)
            return False
        data, body = self.load()
        if data.get('consecutive_failures'):
            data.update(consecutive_failures=0, next_retry_at=None, last_error=None)
            self.save(data, body)
        return sent

    def status(self, error=None):
        data, body = self.load()
        error = error or data.get('last_error')
        try: remaining = max(0, self.audit.stat().st_size - data['offset'])
        except OSError: remaining = None
        return {'state': 'retrying' if error else ('pending' if body else 'running'),
                'updated': time.time(), 'pid': os.getpid(), 'endpoint': self.endpoint,
                'source': self.source, 'delivered_events': data['delivered_events'],
                'delivered_batches': data['delivered_batches'], 'unshipped_bytes': remaining,
                'last_receipt': data.get('last_receipt'), 'last_delivered': data.get('last_delivered'),
                'pending_events': data.get('pending', {}).get('count', 0),
                'pending_bytes': len(body) if body else 0,
                'consecutive_failures': data.get('consecutive_failures', 0),
                'last_attempt_at': data.get('last_attempt_at'),
                'next_retry_at': data.get('next_retry_at'), 'error': error}


def running(state):
    with (Path(state) / 'siem.lock').open('a') as lock:
        try: fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError: return True
    return False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--once', action='store_true', help='Drain available complete events and exit; fail on delivery errors')
    args = parser.parse_args()
    os.umask(0o077)
    lock = (args.state / 'siem.lock').open('a')
    try: fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError: raise SystemExit('SIEM shipper already running') from None
    stop = threading.Event()
    for sig in (signal.SIGTERM, signal.SIGINT): signal.signal(sig, lambda *_: stop.set())
    shipper, failures, previous_error = None, 0, None
    try:
        while not stop.is_set():
            error, sent = None, False
            try:
                if shipper is None: shipper = Shipper(args.state)
                sent = shipper.poll()
                error = shipper.status()['error']
                failures = 0
            except Exception as exc:
                error = error_message(exc)
                failures += 1
            status = shipper.status(error) if shipper else {'state': 'retrying', 'error': error, 'updated': time.time(), 'pid': os.getpid()}
            tmp = args.state / 'siem-status.tmp'
            tmp.write_text(encode(status) + '\n')
            os.replace(tmp, args.state / 'siem-status.json')
            if error != previous_error:
                print(error or 'SIEM delivery recovered', flush=True)
                previous_error = error
            if args.once:
                if error: raise SystemExit(error)
                if not sent: break
            if error:
                delay = shipper.retry_wait() if shipper else 0
                stop.wait(delay or min(60, 2 ** min(failures, 6) + random.random()))
            elif not sent: stop.wait(2)
    finally:
        if shipper: shipper.close()
        lock.close()


if __name__ == '__main__': main()
