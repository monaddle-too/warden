#!/usr/bin/env python3
"""Run as root once; add secrets without replacing existing credentials."""
import os
from pathlib import Path
import secrets
root = Path('/opt/ocsf')
config = root / 'config.env'
existing = config.read_text()
known = {line.split('=', 1)[0] for line in existing.splitlines() if '=' in line}
with config.open('a') as out:
    for key in ('CLICKHOUSE_PASSWORD', 'OCSF_ADMIN_TOKEN', 'OCSF_READ_TOKEN', 'OCSF_INGEST_TOKEN'):
        if key not in known:
            out.write(f'{key}={secrets.token_urlsafe(48)}\n')
config.chmod(0o600)
data = root / 'data'
data.mkdir(exist_ok=True, mode=0o700)
os.chown(data, 65532, 65532)
print('Storage directory and distinct credentials provisioned; existing keys preserved.')
