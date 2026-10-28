"""Embedding provider settings and bounded, resumable document re-indexing."""
import hashlib
import json
import math
import os
from urllib.parse import urlparse

import aiohttp
from fastapi import HTTPException
from pydantic import BaseModel, Field
import secret_store

DEFAULTS = {
    'embedding_url': 'EMBEDDING_BASE_URL',
    'embedding_model': 'EMBEDDING_MODEL',
    'embedding_api_key': 'EMBEDDING_API_KEY',
    'embedding_dimensions': None,
}
KEYS = tuple(DEFAULTS)
LOCK = 719211
SCHEMA = """
SELECT pg_advisory_xact_lock(hashtext('prismvox:embedding-schema'));
DO $$ BEGIN
    IF to_regclass('rag_chunks') IS NOT NULL THEN
        EXECUTE format('DROP INDEX IF EXISTS %I.rag_chunks_embedding_idx', current_schema());
        IF EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid='rag_chunks'::regclass
                   AND attname='embedding' AND atttypmod <> -1) THEN
            ALTER TABLE rag_chunks ALTER COLUMN embedding TYPE vector USING embedding::vector;
        END IF;
        ALTER TABLE rag_chunks ADD COLUMN IF NOT EXISTS embedding_key TEXT NOT NULL DEFAULT '';
        CREATE INDEX IF NOT EXISTS rag_chunks_embedding_key_idx ON rag_chunks(embedding_key);
    END IF;
END $$;
"""


async def migrate(pool):
    async with pool.acquire() as conn:
        async with conn.transaction():
            await conn.execute(SCHEMA)


async def settings(pool):
    rows = await pool.fetch('SELECT key,value FROM config WHERE key = ANY($1::text[])', list(KEYS))
    stored = {r['key']: r['value'] for r in rows}
    cfg = {k: str(stored.get(k, os.getenv(env, '') if env else '')).strip() for k, env in DEFAULTS.items()}
    cfg['embedding_url'] = cfg['embedding_url'].rstrip('/')
    try:
        cfg['embedding_dimensions'] = int(cfg['embedding_dimensions'] or 0)
        cfg['embedding_api_key'] = secret_store.decrypt(cfg['embedding_api_key'], 'embedding_api_key')
    except Exception:
        raise HTTPException(503, 'Embedding configuration unavailable; check the persistent secret key') from None
    return cfg


def validate(cfg):
    parsed = urlparse(cfg['embedding_url'])
    if parsed.scheme not in ('http', 'https') or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise HTTPException(400, 'Enter an HTTP(S) embedding API root without credentials, query or fragment')
    if not cfg['embedding_model'] or len(cfg['embedding_model']) > 512:
        raise HTTPException(400, 'Enter an embedding model ID')


def index_key(cfg):
    text = f"{cfg['embedding_url']}\n{cfg['embedding_model']}\n{cfg['embedding_dimensions']}"
    return hashlib.sha256(text.encode()).hexdigest()


async def embed(cfg, texts):
    if not texts:
        return []
    validate(cfg)
    headers = {'Authorization': 'Bearer '+cfg['embedding_api_key']} if cfg['embedding_api_key'] else {}
    payload = {'model': cfg['embedding_model'], 'input': texts, 'encoding_format': 'float'}
    try:
        async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=30)) as session:
            async with session.post(cfg['embedding_url']+'/embeddings', json=payload, headers=headers, allow_redirects=False) as response:
                if response.status // 100 != 2:
                    raise HTTPException(502, f'Embedding provider returned HTTP {response.status}; check URL, model and credential')
                raw = bytearray()
                async for chunk in response.content.iter_chunked(65536):
                    raw.extend(chunk)
                    if len(raw) > 16*1024*1024:
                        raise ValueError('oversized response')
                data = json.loads(raw)['data']
    except HTTPException:
        raise
    except (aiohttp.ClientError, TimeoutError):
        raise HTTPException(502, 'Embedding endpoint unavailable; check the API URL and network') from None
    except Exception:
        raise HTTPException(502, 'Invalid embedding response') from None
    try:
        if len(data) != len(texts):
            raise ValueError()
        vectors = [None]*len(texts)
        dimension = cfg['embedding_dimensions']
        for item in data:
            idx, vec = item['index'], item['embedding']
            if type(idx) is not int or idx < 0 or idx >= len(texts) or vectors[idx] is not None:
                raise ValueError()
            if not isinstance(vec, list) or not 0 < len(vec) <= 16000:
                raise ValueError()
            dimension = dimension or len(vec)
            if len(vec) != dimension or not all(type(v) in (int,float) and math.isfinite(v) for v in vec) or not any(vec):
                raise ValueError()
            vectors[idx] = vec
        return vectors
    except Exception:
        raise HTTPException(502, 'Invalid embedding vectors or changed dimensions; test the provider and re-index documents') from None


def signature(cfg):
    return tuple(cfg[k] for k in KEYS)


async def prepared(pool):
    cfg = await settings(pool)
    validate(cfg)
    if not cfg['embedding_dimensions']:
        vecs = await embed(cfg, ['PrismVox'])
        async with pool.acquire() as conn:
            async with conn.transaction():
                await conn.execute('SELECT pg_advisory_xact_lock($1)', LOCK)
                if signature(await settings(conn)) != signature(cfg):
                    raise HTTPException(409, 'Embedding settings changed; retry')
                cfg['embedding_dimensions'] = len(vecs[0])
                for key,value in cfg.items():
                    value = secret_store.encrypt(value,key) if key == 'embedding_api_key' else str(value)
                    await conn.execute('INSERT INTO config(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=$2,updated_at=NOW()',key,value)
        cfg['embedding_dimensions'] = len(vecs[0])
    return cfg


async def status(pool, cfg=None):
    cfg = cfg or await settings(pool)
    row = await pool.fetchrow('SELECT count(*) AS total, count(*) FILTER (WHERE embedding IS NOT NULL AND embedding_key=$1) AS indexed FROM rag_chunks', index_key(cfg))
    return {'total': row['total'], 'indexed': row['indexed'], 'pending': row['total']-row['indexed']}


class ProviderUpdate(BaseModel):
    values: dict[str,str] = Field(default_factory=dict)
    clear_api_key: bool = False


def merge(cfg, body):
    if set(body.values) - {'embedding_url','embedding_model','embedding_api_key'}:
        raise HTTPException(400, 'Unknown embedding setting')
    cfg = dict(cfg)
    new_url = body.values.get('embedding_url', cfg['embedding_url']).strip().rstrip('/')
    supplied_key = body.values.get('embedding_api_key', '').strip()
    if new_url != cfg['embedding_url'] or body.clear_api_key:
        cfg['embedding_api_key'] = ''
    cfg['embedding_url'] = new_url
    cfg['embedding_model'] = body.values.get('embedding_model', cfg['embedding_model']).strip()
    if supplied_key:
        cfg['embedding_api_key'] = supplied_key
    cfg['embedding_dimensions'] = 0  # Discover the provider's actual output size.
    return cfg


class ReindexRequest(BaseModel):
    after_id: int = Field(default=0, ge=0)
    limit: int = Field(default=32, ge=1, le=64)


def install(app, pool_getter):
    @app.get('/api/embeddings')
    async def get_provider():
        pool = pool_getter()
        cfg = await settings(pool)
        return {**{k:v for k,v in cfg.items() if k != 'embedding_api_key'},
                'api_key_configured': bool(cfg['embedding_api_key']),
                'configured': bool(cfg['embedding_url'] and cfg['embedding_model']),
                **await status(pool,cfg)}

    @app.post('/api/embeddings/test')
    async def test_provider(body: ProviderUpdate):
        cfg = merge(await settings(pool_getter()),body)
        vectors = await embed(cfg,['PrismVox'])
        return {'ok': True, 'dimensions': len(vectors[0])}

    @app.put('/api/embeddings')
    async def save_provider(body: ProviderUpdate):
        pool = pool_getter()
        cfg = merge(await settings(pool),body)
        vectors = await embed(cfg,['PrismVox'])
        cfg['embedding_dimensions'] = len(vectors[0])
        async with pool.acquire() as conn:
            async with conn.transaction():
                await conn.execute('SELECT pg_advisory_xact_lock($1)', LOCK)
                for key,value in cfg.items():
                    value = secret_store.encrypt(value,key) if key == 'embedding_api_key' else str(value)
                    await conn.execute('INSERT INTO config(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=$2,updated_at=NOW()',key,value)
        return {'ok': True, 'dimensions': cfg['embedding_dimensions'], **await status(pool,cfg)}

    @app.post('/api/rag/embed-all')
    async def reindex(body: ReindexRequest | None = None):
        body = body or ReindexRequest()
        pool = pool_getter()
        cfg = await prepared(pool)
        rows = await pool.fetch('SELECT id,content FROM rag_chunks WHERE id>$1 ORDER BY id LIMIT $2',body.after_id,body.limit)
        vectors = await embed(cfg,[r['content'] for r in rows])
        async with pool.acquire() as conn:
            async with conn.transaction():
                await conn.execute('SELECT pg_advisory_xact_lock($1)', LOCK)
                if signature(await settings(conn)) != signature(cfg):
                    raise HTTPException(409, 'Embedding settings changed; restart re-indexing')
                for row,vec in zip(rows,vectors):
                    await conn.execute('UPDATE rag_chunks SET embedding=$1::vector,embedding_key=$2 WHERE id=$3 AND content=$4',str(vec),index_key(cfg),row['id'],row['content'])
        last = rows[-1]['id'] if rows else body.after_id
        more = await pool.fetchval('SELECT EXISTS(SELECT 1 FROM rag_chunks WHERE id>$1)', last)
        return {'embedded':len(rows),'next_after':last if more else None,'index_key':index_key(cfg),**await status(pool,cfg)}
