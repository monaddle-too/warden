from concurrent.futures import ThreadPoolExecutor
import unittest
from warden.clipboard import ClipboardTransfer, MAX_BYTES

class ClipboardTests(unittest.TestCase):
    def setUp(self):
        self.now = 100
        self.mailbox = ClipboardTransfer(lambda: self.now)
    def tearDown(self): self.mailbox.cancel()
    def test_unicode_multiline_and_plain_text_roundtrip(self):
        for text in ['hello\n世界 👩🏽‍💻\r\nمرحبا\te\u0301\u2028', '{\\rtf1 literal text}', '<script>alert(1)</script>', '', '\ufeffBOM\x00NUL']:
            transfer_id=self.mailbox.send(text)['id']
            self.assertNotIn(text, self.mailbox.snapshot().values())
            self.assertEqual(self.mailbox.take(), {'text': text, 'id': transfer_id})
            self.assertEqual(self.mailbox.take(), {'text': None, 'id': None})
    def test_concurrent_delivery_is_single_use(self):
        self.mailbox.send('one transfer')
        with ThreadPoolExecutor(max_workers=12) as pool:
            replies = list(pool.map(lambda _: self.mailbox.take()['text'], range(30)))
        self.assertEqual(replies.count('one transfer'), 1)
    def test_expiry_cancellation_replacement_and_old_timer(self):
        self.mailbox.send('old'); old = self.mailbox.generation
        self.mailbox.send('new'); self.mailbox._expire(old)
        self.assertEqual(self.mailbox.take()['text'], 'new')
        self.mailbox.send('expiring'); self.now += 60
        self.assertIsNone(self.mailbox.take()['text'])
        self.mailbox.send('cancel'); self.mailbox.cancel()
        self.assertIsNone(self.mailbox.take()['text'])
        self.mailbox.send('timer'); self.mailbox._expire(self.mailbox.generation)
        self.assertIsNone(self.mailbox.text)
    def test_utf8_limits_and_invalid_data_fail_without_replacing(self):
        self.mailbox.send('a' * MAX_BYTES)
        for text in ['a'*(MAX_BYTES+1), '🌍'*(MAX_BYTES//4+1), '\ud800', None, {'text':'oops'}]:
            with self.assertRaises((ValueError, UnicodeError)): self.mailbox.send(text)
        self.assertEqual(len(self.mailbox.take()['text']), MAX_BYTES)
    def test_acknowledgement_requires_current_collected_unexpired_transfer(self):
        first=self.mailbox.send('first')['id']
        self.assertFalse(self.mailbox.acknowledge(first)['accepted'])
        self.mailbox.take()
        self.assertFalse(self.mailbox.acknowledge('wrong')['accepted'])
        self.assertTrue(self.mailbox.acknowledge(first)['accepted'])
        self.assertEqual(self.mailbox.snapshot(first)['status'],'delivered')
        self.assertFalse(self.mailbox.acknowledge(first)['accepted'])
        second=self.mailbox.send('second')['id']; self.mailbox.take()
        self.assertEqual(self.mailbox.cancel(first)['status'],'unknown')
        self.assertFalse(self.mailbox.acknowledge(first)['accepted'])
        self.now += 60
        self.assertFalse(self.mailbox.acknowledge(second)['accepted'])
        self.assertEqual(self.mailbox.snapshot(second)['status'],'expired')
    def test_cancelled_transfer_cannot_trigger_paste(self):
        transfer_id=self.mailbox.send('cancel')['id']; self.mailbox.take()
        self.mailbox.cancel(transfer_id)
        self.assertFalse(self.mailbox.acknowledge(transfer_id)['accepted'])
