"""Bounded, read-only guest resource sampling, separate from authorization.

CPU is normalized across all guest vCPUs. macOS RAM excludes free, inactive and
speculative pages; Linux uses MemTotal - MemAvailable. These are guest-reported
working-memory estimates, not host process RSS or security attestations.
"""
import json
import os
import re
import selectors
import signal
import socket
import subprocess
import threading
import time

INTERVAL = 5
MAX_AGE = 20
LIMIT = 16384
MAC_COMMAND = '/usr/bin/printf "WARDEN_CONFIG\\n"; /usr/sbin/sysctl -n hw.memsize hw.logicalcpu; /usr/bin/top -l 2 -s 1 -n 0; /usr/bin/vm_stat'
LINUX_COMMAND = 'cat /proc/sys/kernel/random/boot_id /proc/stat /proc/meminfo'


def sample(cpu, used, total, cpus):
    if not (0 <= cpu <= 100 and 0 <= used <= total and 0 < total <= 16 * 1024**4 and 1 <= cpus <= 4096):
        raise ValueError('invalid resource sample')
    return {'cpu_percent': round(cpu, 1), 'memory_used_bytes': used,
            'memory_total_bytes': total, 'vcpu_count': cpus}


def parse_mac(text):
    config = re.search(r'WARDEN_CONFIG\s+(\d+)\s+(\d+)\s', text)
    # The first top sample is a lifetime average; only use the second interval.
    idle = re.findall(r'CPU usage:.*?([\d.]+)% idle', text)
    page = re.search(r'page size of (\d+) bytes', text)
    if not config or len(idle) != 2 or not page:
        raise ValueError('incomplete macOS metrics')
    total, cpus = map(int, config.groups())
    pages = {}
    for name in ('free', 'inactive', 'speculative'):
        match = re.search(r'^Pages '+name+r':\s+(\d+)\.', text, re.M)
        if not match: raise ValueError('missing memory counter')
        pages[name] = int(match[1])
    used = total - sum(pages.values()) * int(page[1])
    return sample(100 - float(idle[-1]), used, total, cpus)


def parse_linux(text, previous=None):
    boot = text.splitlines()[0]
    if not re.fullmatch(r'[a-f0-9-]{36}', boot): raise ValueError('invalid boot identity')
    counters = re.search(r'^cpu\s+([\d ]+)$', text, re.M)
    if not counters: raise ValueError('missing CPU counters')
    ticks = [int(n) for n in counters[1].split()][:8]  # guest time is already in user/nice
    if len(ticks) != 8: raise ValueError('incomplete CPU counters')
    total_ticks, idle_ticks = sum(ticks), ticks[3] + ticks[4]
    current = (boot, total_ticks, idle_ticks)
    if previous is None or previous[0] != boot: return None, current
    elapsed, idle = total_ticks - previous[1], idle_ticks - previous[2]
    if elapsed <= 0 or not 0 <= idle <= elapsed: return None, current
    memory = {}
    for name in ('MemTotal', 'MemAvailable'):
        match = re.search(r'^'+name+r':\s+(\d+) kB$', text, re.M)
        if not match: raise ValueError('missing memory counter')
        memory[name] = int(match[1]) * 1024
    cpus = len(re.findall(r'^cpu\d+\s', text, re.M))
    return sample(100 * (elapsed - idle) / elapsed,
                  memory['MemTotal'] - memory['MemAvailable'], memory['MemTotal'], cpus), current


def bounded_command(argv, timeout=7):
    """Cap guest output and kill SSH plus its relay on timeout or overflow."""
    process = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                               stdin=subprocess.DEVNULL, start_new_session=True)
    output = bytearray(); deadline = time.monotonic() + timeout
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0: raise TimeoutError('metrics timeout')
                if not selector.select(remaining): raise TimeoutError('metrics timeout')
                chunk = os.read(process.stdout.fileno(), LIMIT + 1 - len(output))
                if not chunk: break
                output.extend(chunk)
                if len(output) > LIMIT: raise ValueError('metrics output too large')
        if process.wait(timeout=max(.01, deadline-time.monotonic())):
            raise ValueError('guest unavailable')
        return output.decode('utf-8', errors='strict')
    finally:
        try: os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError: pass
        process.wait(); process.stdout.close()


class ResourceMonitor:
    def __init__(self, state):
        self.state = state
        self.lock = threading.Lock()
        self.values = {name: {'status': 'sampling'} for name in ('macos', 'proxy')}
        self.previous = None

    def snapshot(self, now=None):
        now = time.time() if now is None else now
        with self.lock:
            result = {name: dict(value) for name, value in self.values.items()}
        for value in result.values():
            if value['status'] == 'ok' and not 0 <= now-value['sampled_at'] <= MAX_AGE:
                value.clear(); value['status'] = 'unavailable'
        return result

    def collect(self, name):
        if name == 'macos':
            from .guest import ssh_options
            return parse_mac(bounded_command(['/usr/bin/ssh', *ssh_options(self.state),
                                              '-T', 'warden-guest', 'export LC_ALL=C; '+MAC_COMMAND]))
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
            conn.settimeout(3)
            conn.connect(str(self.state/'vm-management.sock'))
            conn.sendall((json.dumps({'argv': ['/bin/sh', '-c', LINUX_COMMAND], 'timeout': 2})+'\n').encode())
            with conn.makefile('rb') as reader: raw = reader.readline(LIMIT+1)
        if len(raw) > LIMIT or not raw.endswith(b'\n'): raise ValueError('invalid metrics response')
        response = json.loads(raw)
        if response.get('exit_code') != 0: raise ValueError('proxy unavailable')
        value, self.previous = parse_linux(response['stdout'], self.previous)
        return value

    def loop(self, name):
        while True:
            start = time.monotonic()
            try:
                value = self.collect(name)
                result = dict(value, status='ok', sampled_at=time.time()) if value else {'status': 'sampling'}
            except Exception:
                if name == 'proxy': self.previous = None
                result = {'status': 'unavailable'}
            with self.lock: self.values[name] = result
            time.sleep(max(.1, INTERVAL-(time.monotonic()-start)))

    def start(self):
        for name in self.values:
            threading.Thread(target=self.loop, args=(name,), daemon=True, name='metrics-'+name).start()
