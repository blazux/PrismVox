"""AES-256-GCM for stored credentials; the key lives outside the database.

Wire format shared with agent/internal/secrets: enc:v1: + base64url(nonce|ciphertext).
The field name is authenticated so encrypted values cannot be swapped between fields.
"""
import base64
import os
import tempfile
from pathlib import Path
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

PREFIX = 'enc:v1:'


def key_path():
    return Path(os.environ.get('SECRET_KEY_PATH', '/var/lib/prismvox/secret.key'))


def load_key(create=False):
    path = key_path()
    if create and not path.exists():
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        fd, temporary = tempfile.mkstemp(prefix='.secret-', dir=path.parent)
        try:
            with os.fdopen(fd, 'wb') as f:
                f.write(os.urandom(32))
                f.flush()
                os.fsync(f.fileno())
            try:
                os.link(temporary, path)  # Publish only a complete key; never overwrite.
            except FileExistsError:
                pass
        finally:
            os.unlink(temporary)
    try:
        data = path.read_bytes()
        if len(data) != 32 or path.stat().st_mode & 0o077:
            raise ValueError('Invalid secret key permissions or length')
        return data
    except OSError as exc:
        raise RuntimeError('Secret key unavailable; restore the persistent key volume') from exc


def encrypt(value, field):
    if not value:
        return ''
    nonce = os.urandom(12)
    payload = nonce + AESGCM(load_key()).encrypt(nonce, value.encode(), field.encode())
    return PREFIX + base64.urlsafe_b64encode(payload).decode().rstrip('=')


def decrypt(value, field):
    if not value or not value.startswith(PREFIX):
        return value or ''  # Read compatibility during coordinated upgrade.
    try:
        raw = value[len(PREFIX):]
        payload = base64.urlsafe_b64decode(raw + '=' * (-len(raw) % 4))
        return AESGCM(load_key()).decrypt(payload[:12], payload[12:], field.encode()).decode()
    except Exception as exc:
        raise RuntimeError('Stored credential cannot be decrypted; check the persistent key') from exc


async def migrate(pool):
    # If ciphertext already exists, require the old key BEFORE creating anything.
    encrypted = await pool.fetchval("SELECT EXISTS(SELECT 1 FROM config WHERE key IN ('sip_password','voice_api_key','voice_service_api_key') AND value LIKE 'enc:v1:%') OR EXISTS(SELECT 1 FROM mcp_servers WHERE auth_token LIKE 'enc:v1:%')")
    load_key(create=not encrypted)
    async with pool.acquire() as conn:
        async with conn.transaction():
            await conn.execute('SELECT pg_advisory_xact_lock(719209)')
            for row in await conn.fetch("SELECT key,value FROM config WHERE key IN ('sip_password','voice_api_key','voice_service_api_key') FOR UPDATE"):
                field, value = row['key'], row['value']
                if value.startswith(PREFIX):
                    decrypt(value, field)
                elif value:
                    await conn.execute("UPDATE config SET value=$1 WHERE key=$2", encrypt(value, field), field)
            for row in await conn.fetch('SELECT id, auth_token FROM mcp_servers WHERE auth_token IS NOT NULL FOR UPDATE'):
                value = row['auth_token']
                field = f"mcp:{row['id']}"
                if value.startswith(PREFIX):
                    decrypt(value, field)
                elif value:
                    await conn.execute('UPDATE mcp_servers SET auth_token=$1 WHERE id=$2', encrypt(value, field), row['id'])
