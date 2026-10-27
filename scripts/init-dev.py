#!/usr/bin/env python3
"""Create local development settings once; never print credentials."""
import os
from pathlib import Path
import secrets

root = Path(__file__).resolve().parent.parent
path = root / '.env.local'
if path.exists():
    print('Keeping existing .env.local')
else:
    password = secrets.token_hex(32)
    content = (
        f'POSTGRES_PASSWORD={password}\nDB_PORT=54329\n'
        f'DATABASE_URL=postgres://xui:{password}@127.0.0.1:54329/xui?sslmode=disable\n'
        'APP_MODE=development\nPUBLIC_ORIGIN=http://127.0.0.1:5173\nHTTP_ADDR=127.0.0.1:8080\n'
    )
    with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as f:
        f.write(content)
    print('Created .env.local with random local database credentials')
