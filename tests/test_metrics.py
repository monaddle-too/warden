import json
from pathlib import Path
import sys
import time
import unittest
from warden.metrics import ResourceMonitor, bounded_command, parse_linux, parse_mac

MAC = '''WARDEN_CONFIG
1073741824
4
CPU usage: 20.0% user, 10.0% sys, 70.0% idle
CPU usage: 5.0% user, 10.0% sys, 85.0% idle
Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free: 1000.
Pages inactive: 2000.
Pages speculative: 100.
'''

def linux(user, idle, boot='a'*36):
    return f'''{boot}
cpu {user} 0 0 {idle} 0 0 0 0 100 0
cpu0 0
cpu1 0
MemTotal: 1048576 kB
MemAvailable: 262144 kB
'''

class ResourceTests(unittest.TestCase):
    def test_mac_uses_interval_and_actual_page_size(self):
        value = parse_mac(MAC)
        self.assertEqual(value['cpu_percent'], 15)
        self.assertEqual(value['memory_used_bytes'], 1073741824 - 3100 * 16384)
        self.assertEqual(value['vcpu_count'], 4)
        with self.assertRaises(ValueError): parse_mac(MAC.replace('Pages inactive:', 'missing:'))
        with self.assertRaises(ValueError): parse_mac(MAC.replace('85.0% idle','185.0% idle'))

    def test_linux_normalizes_all_cpus_and_handles_reboot(self):
        value, prior = parse_linux(linux(100,100))
        self.assertIsNone(value)
        value, current = parse_linux(linux(150,250), prior)
        self.assertEqual(value['cpu_percent'],25)
        self.assertEqual(value['vcpu_count'],2)
        self.assertEqual(value['memory_used_bytes'],768*1024**2)
        self.assertIsNone(parse_linux(linux(150,250), current)[0])
        self.assertIsNone(parse_linux(linux(150,250,'b'*36), prior)[0])
        self.assertIsNone(parse_linux(linux(50,50), prior)[0])

    def test_unavailable_and_stale_do_not_show_zero_or_old_values(self):
        monitor = ResourceMonitor(Path('/nonexistent'))
        self.assertEqual(monitor.snapshot()['macos']['status'],'sampling')
        monitor.values['macos'] = dict(parse_mac(MAC),status='ok',sampled_at=100)
        self.assertEqual(monitor.snapshot(now=110)['macos']['status'],'ok')
        self.assertEqual(monitor.snapshot(now=121)['macos'],{'status':'unavailable'})
        self.assertEqual(monitor.snapshot(now=99)['macos'],{'status':'unavailable'})

    def test_bounded_output_and_timeout(self):
        self.assertEqual(bounded_command([sys.executable,'-c','print("ok")']), 'ok\n')
        with self.assertRaises(ValueError):
            bounded_command([sys.executable,'-c','print("x"*20000)'])
        start=time.monotonic()
        with self.assertRaises(TimeoutError):
            bounded_command([sys.executable,'-c','import time; time.sleep(20)'],timeout=.1)
        self.assertLess(time.monotonic()-start,2)
