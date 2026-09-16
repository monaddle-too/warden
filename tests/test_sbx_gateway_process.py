"""Loopback-only process smoke; default unverified broker must block all egress."""
import http.client
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest

from warden.sbx import Registry, Server, handler, rpc
from warden.sbx_gateway import command
from test_sbx import run_context

MITMDUMP = Path(sys.executable).parent / 'mitmdump'


@unittest.skipUnless(MITMDUMP.is_file(), 'pinned mitmdump executable required')
class GatewayProcessTests(unittest.TestCase):
    def test_module_entrypoint_managed_gateway_accepts_shared_proof_type(self):
        with tempfile.TemporaryDirectory(prefix='w-sbx-entry-', dir='/tmp') as temp:
            root = Path(temp)
            cli = root / 'sbx-fixture'
            cli.write_text('#!' + sys.executable + '\nimport sys\nfrom test_sbx_verifier import CliFixture\nf=CliFixture()\nf.runtime=False\nprint(f(sys.argv[1:]))\n')
            cli.chmod(0o700)
            repository = Path(__file__).resolve().parents[1]
            environment = {**os.environ, 'PYTHONPATH': os.pathsep.join((str(repository / 'host'), str(repository / 'tests')))}
            log = (root / 'service.log').open('w+')
            process = subprocess.Popen([sys.executable, '-m', 'warden.sbx', '--state', str(root / 'state'),
                '--sbx', str(cli), '--mitmdump', str(MITMDUMP)], env=environment, stdout=log, stderr=log)
            path = root / 'state/sbx-control.sock'
            try:
                deadline = time.monotonic() + 5
                while not path.exists() and process.poll() is None and time.monotonic() < deadline:
                    time.sleep(0.05)
                self.assertTrue(path.exists())
                value = run_context()
                self.assertTrue(rpc(path, {'version': 1, 'operation': 'register', 'context': value})['ok'])
                result = rpc(path, {'version': 1, 'operation': 'check', 'context': value, 'phase': 'create'})
                self.assertTrue(result['ready'], result)
                self.assertFalse(rpc(path, {'version': 1, 'operation': 'check', 'context': value, 'phase': 'runtime'})['ready'])
            finally:
                process.terminate()
                try: process.wait(timeout=5)
                except subprocess.TimeoutExpired: process.kill();process.wait(timeout=5)
                log.close()

    def test_unverified_gateway_rejects_real_http_connect_and_raw_socket(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            registry = Registry(root / 'broker')
            value = run_context()
            registry.register(value)
            with socket.socket() as allocation:
                allocation.bind(('127.0.0.1', 0))
                port = allocation.getsockname()[1]
            registry.bind_gateway(value, port)
            control_path = root / 'control.sock'
            server = Server(str(control_path), handler(registry))
            control_path.chmod(0o600)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            config = root / 'gateway.json'
            config.write_text(json.dumps({**registry.gateway(value), 'socketPath': str(control_path)}))
            config.chmod(0o600)
            log = open(root / 'gateway.log', 'w+')
            process = subprocess.Popen(command(str(MITMDUMP), root, port),
                env={**os.environ, 'WARDEN_SBX_GATEWAY_CONFIG': str(config)}, stdout=log, stderr=log)
            try:
                deadline = time.monotonic() + 12
                while True:
                    try:
                        with socket.create_connection(('127.0.0.1', port), timeout=0.2): break
                    except OSError:
                        if process.poll() is not None or time.monotonic() >= deadline:
                            log.flush();log.seek(0)
                            self.fail('gateway did not start: ' + log.read()[-3000:])
                        time.sleep(0.05)
                connection = http.client.HTTPConnection('127.0.0.1', port, timeout=3)
                connection.request('POST', '/openai/v1/responses', b'{}', {'Host': 'host.docker.internal:' + str(port), 'Content-Type': 'application/json'})
                response = connection.getresponse()
                self.assertEqual(response.status, 503)
                response.read();connection.close()
                with socket.create_connection(('127.0.0.1', port), timeout=3) as client:
                    client.sendall(b'CONNECT api.openai.com:443 HTTP/1.1\r\nHost: api.openai.com:443\r\n\r\n')
                    self.assertIn(b' 403 ', client.recv(4096))
                with socket.create_connection(('127.0.0.1', port), timeout=3) as client:
                    client.sendall(b'SSH-2.0-attacker\r\n\r\n')
                    data = client.recv(4096)
                    self.assertTrue(not data or b'400' in data)
            finally:
                process.terminate()
                try: process.wait(timeout=5)
                except subprocess.TimeoutExpired: process.kill();process.wait(timeout=5)
                log.close()
                server.shutdown();server.server_close();registry.close()


if __name__ == '__main__':
    unittest.main()
