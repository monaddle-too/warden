#!/usr/bin/env python3
"""Bounded, manual Linux-appliance diagnostic; never run on the host.

Run through proxy-exec in a disposable appliance copy. It does not change rules.
Optional memory load is capped at 80% of currently available guest RAM.
"""
import argparse
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import platform
import struct
import subprocess
import sys
import time

def module_code():
    try:
        address = getattr(module_code, 'address', None)
        if address is None:
            with open('/proc/kallsyms') as symbols:
                for line in symbols:
                    fields = line.split()
                    if len(fields) == 4 and fields[2:] == ['nfnetlink_rcv', '[nfnetlink]']:
                        address = int(fields[0], 16); break
            if not address: return {'error': 'nfnetlink_rcv address unavailable'}
            module_code.address = address
        with open('/proc/kcore', 'rb', buffering=0) as stream:
            header = stream.read(64)
            offset = struct.unpack_from('<Q', header, 32)[0]
            size, count = struct.unpack_from('<HH', header, 54)
            stream.seek(offset)
            segments = stream.read(size * count)
            for i in range(count):
                kind, flags, file_offset, virtual, physical, file_size, mem_size, align = struct.unpack_from('<IIQQQQQQ', segments, i * size)
                if kind == 1 and virtual <= address and address + 4096 <= virtual + file_size:
                    stream.seek(file_offset + address - virtual)
                    code = stream.read(4096)
                    return {'address': hex(address), 'bytes': len(code), 'sha256': hashlib.sha256(code).hexdigest(), 'first32': code[:32].hex()}
        return {'error': 'module address absent from kcore segments'}
    except (OSError, ValueError) as error:
        return {'error': str(error)}

def snapshot():
    return {'time': time.time(), 'taint': Path('/proc/sys/kernel/tainted').read_text().strip(),
            'memory': Path('/proc/meminfo').read_text(), 'pressure': Path('/proc/pressure/memory').read_text(),
            'module_code': module_code()}

def main():
    if platform.system() != 'Linux' or not Path('/sys/module/nfnetlink').exists():
        raise SystemExit('Requires the disposable Linux enforcement appliance')
    parser = argparse.ArgumentParser()
    parser.add_argument('--seconds', type=int, default=60)
    parser.add_argument('--memory-mib', type=int, default=0)
    parser.add_argument('--workers', type=int, default=4)
    args = parser.parse_args()
    if not 1 <= args.seconds <= 120 or not 1 <= args.workers <= 8 or args.memory_mib < 0:
        raise SystemExit('Invalid bounded test parameters')
    result = {'kernel': platform.release(), 'parameters': vars(args), 'before': snapshot()}
    memory = None
    try:
        if args.memory_mib:
            available = int(next(line.split()[1] for line in Path('/proc/meminfo').read_text().splitlines() if line.startswith('MemAvailable:'))) // 1024
            amount = min(args.memory_mib, int(available * .8))
            result['allocated_mib'] = amount
            program = '''import mmap,sys,time
n=int(sys.argv[1]); region=mmap.mmap(-1,n*1024*1024)
block=bytes(range(256))*4096
for i in range(n): region[i*1048576:(i+1)*1048576]=block
print('ready',flush=True)
time.sleep(int(sys.argv[2]))
for i in range(n):
    assert region[i*1048576:(i+1)*1048576]==block, ('memory mismatch',i)
print('verified',flush=True)
'''
            memory = subprocess.Popen([sys.executable, '-c', program, str(amount), str(args.seconds + 3)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            if memory.stdout.readline().strip() != 'ready': raise RuntimeError('memory worker failed to allocate')
        start = time.monotonic()
        def query_loop():
            count = 0
            while time.monotonic() - start < args.seconds:
                try:
                    proc = subprocess.run(['/usr/sbin/nft', 'list', 'table', 'inet', 'warden'], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=8)
                except subprocess.TimeoutExpired:
                    return {'queries': count, 'exit': 'timeout', 'stderr': 'nft exceeded eight seconds'}
                if proc.returncode: return {'queries': count, 'exit': proc.returncode, 'stderr': proc.stderr.decode(errors='replace')}
                count += 1
                time.sleep(.01)
            return {'queries': count, 'exit': 0}
        samples = []
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as pool:
            futures = [pool.submit(query_loop) for _ in range(args.workers)]
            while any(not f.done() for f in futures):
                samples.append(snapshot())
                time.sleep(5)
            result['workers'] = [f.result() for f in futures]
        result['samples'] = samples
        if memory:
            output, errors = memory.communicate(timeout=15)
            result['memory_worker'] = {'exit': memory.returncode, 'stdout': output, 'stderr': errors}
        result['after'] = snapshot()
        hashes = [s['module_code'].get('sha256') for s in [result['before'], *samples, result['after']]]
        result['module_code_consistent'] = None not in hashes and len(set(hashes)) == 1
        result['passed'] = all(w['exit'] == 0 for w in result['workers']) and result['after']['taint'] == '0' and result['module_code_consistent'] and (not memory or memory.returncode == 0)
        print(json.dumps(result))
        return 0 if result['passed'] else 1
    finally:
        if memory and memory.poll() is None:
            memory.kill(); memory.wait()

if __name__ == '__main__':
    raise SystemExit(main())
