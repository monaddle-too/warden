"""One explicit host-to-guest text transfer; never reads the host clipboard."""
import threading
import time
import uuid

MAX_BYTES = 64 * 1024
TTL = 60

class ClipboardTransfer:
    def __init__(self, clock=time.monotonic):
        self.clock = clock
        self.lock = threading.Lock()
        self.text = None
        self.expires = 0
        self.timer = None
        self.generation = 0
        self.status = 'idle'
        self.transfer_id = None

    def _clear(self, status):
        self.text = None
        self.status = status
        if self.timer:
            self.timer.cancel()
            self.timer = None

    def _expire(self, generation):
        with self.lock:
            if self.generation == generation:
                self._clear('expired')

    def send(self, text, transfer_id=None):
        if not isinstance(text, str) or len(text.encode('utf-8')) > MAX_BYTES:
            raise ValueError('text exceeds clipboard limit')
        transfer_id = str(uuid.UUID(transfer_id)) if transfer_id is not None else str(uuid.uuid4())
        with self.lock:
            self._clear('pending')
            self.generation += 1
            self.transfer_id = transfer_id
            self.text = text
            self.expires = self.clock() + TTL
            self.timer = threading.Timer(TTL, self._expire, (self.generation,))
            self.timer.daemon = True
            self.timer.start()
        return {'id': transfer_id, 'status': 'pending', 'expires_in': TTL}

    def take(self):
        with self.lock:
            if self.text is not None and self.clock() >= self.expires:
                self._clear('expired')
            text = self.text
            if text is not None:
                # Drop the payload immediately, but retain the expiry timer until
                # the matching guest acknowledgement arrives.
                self.text = None
                self.status = 'collected'
            return {'text': text, 'id': self.transfer_id if text is not None else None}

    def acknowledge(self, transfer_id):
        with self.lock:
            if transfer_id != self.transfer_id or self.status != 'collected' or self.clock() >= self.expires:
                return {'accepted': False}
            self.generation += 1
            self._clear('delivered')
            return {'accepted': True}

    def cancel(self, transfer_id=None):
        with self.lock:
            if transfer_id is not None and transfer_id != self.transfer_id:
                return {'status': 'unknown'}
            self.generation += 1
            self._clear('cancelled')
        return {'status': 'cancelled'}

    def snapshot(self, transfer_id=None):
        with self.lock:
            if transfer_id is not None and transfer_id != self.transfer_id:
                return {'id': transfer_id, 'status': 'unknown'}
            if self.status in ('pending', 'collected') and self.clock() >= self.expires:
                self._clear('expired')
            return {'id': self.transfer_id, 'status': self.status}
