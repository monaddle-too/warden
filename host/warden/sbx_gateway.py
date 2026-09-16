"""Launch the inspected loopback-only gateway from private host context JSON."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile

from .core import ROOT, dumps
from .sbx import context, rpc


def command(executable, state, port):
    return [executable, '--quiet', '--mode', 'regular', '--listen-host', '127.0.0.1',
            '--listen-port', str(port), '--set', 'confdir=' + str(state / 'ca'),
            '--set', 'connection_strategy=lazy', '--set', 'upstream_cert=false',
            '--set', 'ssl_insecure=false', '--set', 'rawtcp=false', '--set', 'websocket=false',
            '--set', 'body_size_limit=128m', '--set', 'flow_detail=0',
            '--set', 'block_global=true', '-s', str(ROOT / 'proxy/sbx_addon.py')]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--socket', type=Path, required=True)
    parser.add_argument('--context', type=Path, required=True, help='host-owned context JSON, not guest data')
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--port', type=int, required=True)
    parser.add_argument('--mitmdump', required=True)
    args = parser.parse_args()
    os.umask(0o077)
    value = context(json.loads(args.context.read_text()))
    rpc(args.socket, {'version': 1, 'operation': 'bindGateway', 'context': value, 'port': args.port})
    binding = rpc(args.socket, {'version': 1, 'operation': 'gateway', 'context': value})
    args.state.mkdir(mode=0o700, parents=True, exist_ok=True)
    args.state.chmod(0o700)
    config = {**binding, 'socketPath': str(args.socket.resolve())}
    with tempfile.NamedTemporaryFile(mode='w', prefix='gateway-', suffix='.json', dir=args.state) as stream:
        stream.write(dumps(config))
        stream.flush()
        environment = {**os.environ, 'WARDEN_SBX_GATEWAY_CONFIG': stream.name}
        raise SystemExit(subprocess.run(command(args.mitmdump, args.state, args.port), env=environment).returncode)


if __name__ == '__main__':
    main()
