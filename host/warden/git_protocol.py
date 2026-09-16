"""Small, fail-closed smart HTTP grammar shared by the host and proxy.

Read sessions permit the upload-pack service only. A write is one SHA-1 branch
update, with no deletes, push options, certificates, tags, or alternate formats.
The exact permission also binds the complete HTTP request, including pack bytes.
"""
import re

ZERO = '0' * 40
REPO = r'([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9_.-]{1,100})'

def route(method, path):
    match = re.fullmatch('/' + REPO + r'\.git/(info/refs\?service=git-(upload|receive)-pack|git-(upload|receive)-pack)', path)
    if not match or match[2] in ('.', '..'): return None
    discovery = match[3].startswith('info/')
    if method != ('GET' if discovery else 'POST'): return None
    return {'repository':match[1]+'/'+match[2], 'write':not discovery and match[5]=='receive', 'discovery':discovery}

def packet(data, offset):
    if not re.fullmatch(b'[0-9a-fA-F]{4}', data[offset:offset+4]): raise ValueError('invalid Git packet length')
    size = int(data[offset:offset+4], 16)
    if size == 0: return None, offset+4
    if size < 4 or size > 65520 or offset+size > len(data): raise ValueError('invalid Git packet bounds')
    return data[offset+4:offset+size], offset+size

def push(data):
    command, offset = packet(data, 0)
    if not command or b'\0' not in command: raise ValueError('Git push requires an explicit capability list')
    line, capabilities = command.split(b'\0', 1)
    # A LF is optional in pkt-lines, but embedded/trailing commands are not.
    capabilities = capabilities.removesuffix(b'\n').removeprefix(b' ')
    for cap in capabilities.split(b' '):
        if cap in (b'report-status', b'report-status-v2', b'side-band-64k', b'quiet', b'ofs-delta', b'atomic', b'object-format=sha1'): continue
        if re.fullmatch(rb'agent=[A-Za-z0-9._/+()-]{1,100}', cap): continue
        raise ValueError('unsupported Git push capability')
    match = re.fullmatch(rb'([0-9a-f]{40}) ([0-9a-f]{40}) (refs/heads/[A-Za-z0-9_./-]{1,200})', line)
    if not match: raise ValueError('only SHA-1 branch pushes are supported')
    old, new, ref = (part.decode('ascii') for part in match.groups())
    if new == ZERO or new == old: raise ValueError('branch deletion and empty updates are unsupported')
    if any(part.startswith('.') or part.endswith(('.', '.lock')) or not part for part in ref.split('/')) or '..' in ref or '@{' in ref:
        raise ValueError('invalid Git branch name')
    end, offset = packet(data, offset)
    if end is not None: raise ValueError('push one branch at a time; multiple ref updates are blocked')
    pack = data[offset:]
    if pack and (not pack.startswith(b'PACK') or len(pack) < 32): raise ValueError('invalid Git pack')
    return {'old':old, 'new':new, 'ref':ref}, pack

def inspect(method, path, headers, body):
    result = route(method, path)
    if not result: raise ValueError('unsupported Git HTTP endpoint; use an HTTPS .git remote')
    headers = {k.lower():v for k,v in headers}
    if headers.get('content-encoding', 'identity') != 'identity': raise ValueError('compressed Git requests are unsupported')
    if result['discovery']:
        if body: raise ValueError('Git discovery must not contain a body')
    else:
        service = 'receive' if result['write'] else 'upload'
        if headers.get('content-type') != 'application/x-git-'+service+'-pack-request': raise ValueError('invalid Git content type')
        if not body: raise ValueError('empty Git service request')
    if result['write']: result['update'], _ = push(body)
    return result
