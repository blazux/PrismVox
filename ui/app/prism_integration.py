"""Saved Prism connection; credentials stay in Vox's encrypted configuration."""
import asyncio
import json
import os
from urllib.parse import urlsplit, urlunsplit

import aiohttp
from fastapi import HTTPException
from pydantic import BaseModel

import secret_store

KEYS = ('prism_enabled', 'prism_url', 'prism_token', 'prism_public_url', 'prism_model')


def defaults():
    url = (os.getenv('PRISM_URL') or os.getenv('CORTEX_URL') or '').rstrip('/')
    return {'prism_enabled': 'true' if url else 'false', 'prism_url': url,
            'prism_token': os.getenv('PRISM_TOKEN') or os.getenv('CORTEX_TOKEN') or '',
            'prism_public_url': os.getenv('PRISM_PUBLIC_URL', ''), 'prism_model': ''}


async def settings(pool=None, *, reveal=False, bootstrap=None):
    cfg = defaults() if bootstrap is None else dict(bootstrap)
    stored = {}
    if pool is not None:
        rows = await pool.fetch('SELECT key,value FROM config WHERE key = ANY($1::text[])', list(KEYS))
        stored = {row['key']: row['value'] for row in rows}
        cfg.update(stored)
    cfg['prism_token'] = secret_store.decrypt(cfg.get('prism_token', ''), 'prism_token')
    cfg['enabled'] = cfg.get('prism_enabled') == 'true'
    cfg['model'] = cfg.pop('prism_model', '')
    cfg.pop('prism_enabled', None)
    cfg['source'] = 'saved' if 'prism_enabled' in stored else 'environment'
    if not reveal:
        cfg['token_configured'] = bool(cfg.pop('prism_token'))
    return cfg


def http_url(value):
    parts = urlsplit(value)
    return urlunsplit(parts._replace(scheme={'ws': 'http', 'wss': 'https'}.get(parts.scheme, parts.scheme))).rstrip('/')


def validate_url(value, label, *, optional=True):
    value = value.strip().rstrip('/')
    if not value and optional:
        return value
    try:
        parsed = urlsplit(value)
        port = parsed.port
    except ValueError:
        raise HTTPException(400, f'{label} is not a valid server address') from None
    if (parsed.scheme not in ('http', 'https') or not parsed.hostname or parsed.username is not None
            or parsed.password is not None or parsed.query or parsed.fragment or len(value) > 2048):
        raise HTTPException(400, f'{label} must use HTTP or HTTPS without credentials, query or fragment')
    return value


class ConnectionUpdate(BaseModel):
    enabled: bool | None = None
    prism_url: str | None = None
    prism_token: str = ''
    clear_prism_token: bool = False
    prism_public_url: str | None = None
    model: str | None = None


def merge(cfg, body):
    cfg = dict(cfg)
    previous_url = cfg['prism_url'].rstrip('/')
    for key in ('enabled', 'prism_url', 'prism_public_url', 'model'):
        value = getattr(body, key)
        if value is not None:
            cfg[key] = value if key == 'enabled' else value.strip()
    cfg['prism_url'] = validate_url(cfg['prism_url'], 'Prism URL')
    if cfg['prism_url'] != previous_url and not body.prism_token.strip():
        cfg['prism_token'] = ''
    if body.prism_token.strip():
        cfg['prism_token'] = body.prism_token.strip()
    if body.clear_prism_token:
        cfg['prism_token'] = ''
    cfg['prism_public_url'] = validate_url(cfg['prism_public_url'], 'Public Prism URL')
    if cfg['enabled'] and (not cfg['prism_url'] or not cfg['prism_token']):
        raise HTTPException(400, 'Enter the Prism URL and connection token before enabling the connection; changing the URL requires a new token')
    if len(cfg['model']) > 512 or len(cfg['prism_token']) > 4096 or any(c in cfg['prism_token'] for c in '\r\n'):
        raise HTTPException(400, 'Invalid Prism model or connection token')
    return cfg


async def migrate(pool):
    values = defaults()
    # Older deployments can expose the Prism URL to the UI while keeping its
    # credential only in the call agent. Leave an absent credential absent so
    # the agent can retain its existing environment connection.
    if not values['prism_url']:
        return
    async with pool.acquire() as conn:
        async with conn.transaction():
            await conn.execute('SELECT pg_advisory_xact_lock(719211)')
            if await conn.fetchval("SELECT EXISTS(SELECT 1 FROM config WHERE key='prism_enabled')"):
                return
            for key, value in values.items():
                if key == 'prism_token':
                    if not value:
                        continue
                    value = secret_store.encrypt(value, key)
                await conn.execute('''INSERT INTO config(key,value,updated_at) VALUES($1,$2,NOW())
                    ON CONFLICT(key) DO NOTHING''', key, value)


async def inference(pool):
    cfg = await settings(pool, reveal=True)
    if cfg['enabled'] and cfg['model']:
        if not cfg['prism_url'] or not cfg['prism_token']:
            raise HTTPException(503, 'Complete the Prism connection before using its model')
        return (http_url(cfg['prism_url']) + '/api/voice/inference/v1',
                {'Authorization': 'Bearer ' + cfg['prism_token']}, cfg['model'])
    return None


async def status(cfg):
    result = {'enabled': cfg['enabled'], 'connected': False, 'inference': False,
              'status': 'disabled', 'detail': 'Prism connection is disabled'}
    if not cfg['enabled']:
        return result
    if not cfg['prism_url'] or not cfg.get('prism_token'):
        return dict(result, status='incomplete', detail='Enter the Prism URL and connection token')
    try:
        async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=3)) as session:
            headers = {'Authorization': 'Bearer ' + cfg['prism_token']}
            url = http_url(cfg['prism_url'])
            async with session.get(url + '/api/voice/capabilities', headers=headers, allow_redirects=False) as response:
                if response.status in (401, 403):
                    return dict(result, status='unauthorized', detail='Prism rejected the connection token')
                # Older Prism installations can retain their existing phone link.
                legacy = response.status == 404
                payload = await response.content.read(65537)
                if len(payload) > 65536:
                    return dict(result, status='incompatible', detail='Prism returned an unexpected connection response')
                if not legacy and response.status != 200:
                    return dict(result, status='unavailable', detail='Prism could not complete the connection check')
            if legacy:
                async with session.get(url + '/api/voice/directory', headers=headers, allow_redirects=False) as response:
                    if response.status in (401, 403):
                        return dict(result, status='unauthorized', detail='Prism rejected the connection token')
                    payload = await response.content.read(65537)
                    if response.status != 200 or len(payload) > 65536:
                        return dict(result, status='incompatible', detail='Update Prism to configure this connection')
                data = json.loads(payload)
                valid = isinstance(data, dict) and isinstance(data.get('entries'), list)
                return dict(result, connected=valid, status='connected' if valid else 'incompatible',
                            detail='Connected; update Prism to select its models' if valid else 'Prism returned an unexpected connection response')
            data = json.loads(payload)
            if not isinstance(data, dict) or data.get('ok') is not True or data.get('service') != 'prism' or data.get('voice') is not True:
                return dict(result, status='incompatible', detail='This address is not a compatible Prism instance')
            return dict(result, connected=True, inference=data.get('inference') is True,
                        status='connected', detail='Connected to Prism')
    except (aiohttp.ClientError, asyncio.TimeoutError, ValueError):
        return dict(result, status='unreachable', detail='Could not reach Prism; check its address and network')


def install(app, pool_getter):
    @app.get('/api/prism/connection')
    async def get_connection():
        return await settings(pool_getter())

    @app.put('/api/prism/connection')
    async def put_connection(body: ConnectionUpdate):
        pool = pool_getter()
        async with pool.acquire() as conn:
            async with conn.transaction():
                await conn.execute('SELECT pg_advisory_xact_lock(719211)')
                cfg = merge(await settings(conn, reveal=True), body)
                values = {'prism_enabled': 'true' if cfg['enabled'] else 'false',
                          'prism_url': cfg['prism_url'], 'prism_public_url': cfg['prism_public_url'],
                          'prism_model': cfg['model'], 'prism_token': secret_store.encrypt(cfg['prism_token'], 'prism_token')}
                for key, value in values.items():
                    await conn.execute('''INSERT INTO config(key,value,updated_at) VALUES($1,$2,NOW())
                        ON CONFLICT(key) DO UPDATE SET value=$2,updated_at=NOW()''', key, value)
        return await settings(pool)

    @app.get('/api/prism/connection/status')
    async def get_status():
        return await status(await settings(pool_getter(), reveal=True))
