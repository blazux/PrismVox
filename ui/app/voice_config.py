"""Speech-provider configuration and ElevenLabs administration.

The database is shared with the call agent. Secrets are write-only and encrypted;
callers see only a per-call snapshot, never changes made halfway through a call.
"""
import asyncio
import io
import json
import math
import os
from pathlib import Path
import wave
from urllib.parse import urlparse, quote

import aiohttp
from fastapi import HTTPException, Response
from pydantic import BaseModel, Field
import secret_store

DEFAULTS = {
    'voice_stt_provider': ('STT_PROVIDER', 'elevenlabs'),
    'voice_tts_provider': ('TTS_PROVIDER', 'elevenlabs'),
    'voice_stt_url': ('STT_URL', 'http://stt:9000'),
    'voice_tts_url': ('TTS_URL', 'http://tts:8000/synthesize'),
    'voice_api_key': ('ELEVENLABS_API_KEY', ''),
    'voice_language': ('ELEVENLABS_LANGUAGE', 'fr'),
    'voice_stt_model': ('ELEVENLABS_STT_MODEL', 'scribe_v2_realtime'),
    'voice_tts_model': ('ELEVENLABS_TTS_MODEL', 'eleven_flash_v2_5'),
    'voice_id': ('ELEVENLABS_VOICE_ID', ''),
    'voice_stability': ('ELEVENLABS_STABILITY', ''),
    'voice_service_api_key': ('SPEECH_SERVICE_API_KEY', ''),
    'voice_service_stt_model': ('STT_MODEL', ''),
    'voice_service_tts_model': ('TTS_MODEL', ''),
    'voice_service_voice': ('TTS_VOICE', ''),
}
KEYS = tuple(DEFAULTS)
SECRET_FIELDS = {'voice_api_key': 'api_key_configured', 'voice_service_api_key': 'service_api_key_configured'}
MODELS_READ_REQUIRED = 'Enable Models Read on your ElevenLabs API key to load the full model list. Your selected model is unchanged.'


async def settings(pool, *, reveal=False):
    rows = await pool.fetch('SELECT key,value FROM config WHERE key = ANY($1::text[])', list(KEYS))
    stored = {row['key']: row['value'] for row in rows}
    cfg = {key: stored.get(key, os.getenv(env) or default) for key, (env, default) in DEFAULTS.items()}
    if cfg['voice_stt_provider'] == 'elevenlabs' and cfg['voice_stt_model'] == 'scribe_v1':
        cfg['voice_stt_model'] = 'scribe_v2'
    if cfg['voice_tts_provider'] == 'service' and urlparse(cfg['voice_tts_url']).path in ('', '/'):
        cfg['voice_tts_url'] = cfg['voice_tts_url'].rstrip('/') + '/synthesize'
    for field, flag in SECRET_FIELDS.items():
        cfg[field] = secret_store.decrypt(cfg[field], field)
        if not reveal:
            cfg[flag] = bool(cfg.pop(field))
    return cfg


class VoiceUpdate(BaseModel):
    values: dict[str, str] = Field(default_factory=dict)
    clear_api_key: bool = False
    clear_service_api_key: bool = False


def validate(cfg, *, complete=True):
    stability = cfg.get('voice_stability', '')
    if stability:
        try:
            valid = math.isfinite(float(stability)) and 0 <= float(stability) <= 1
        except ValueError:
            valid = False
        if not valid:
            raise HTTPException(400, 'Voice stability must be between 0 and 1, or empty to use the provider setting')
    for kind in ('stt', 'tts'):
        if cfg[f'voice_{kind}_provider'] not in ('service', 'elevenlabs', 'openai'):
            raise HTTPException(400, 'Choose a supported speech provider')
        parsed = urlparse(cfg[f'voice_{kind}_url'])
        if parsed.scheme not in ('http', 'https') or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
            raise HTTPException(400, 'Service URLs must use HTTP or HTTPS without embedded credentials')
    if len(cfg['voice_language']) > 3:
        raise HTTPException(400, 'Use a two- or three-letter language code, or leave it empty for automatic detection')
    if 'elevenlabs' in (cfg['voice_stt_provider'], cfg['voice_tts_provider']):
        if not cfg['voice_api_key']:
            raise HTTPException(400, 'Enter your ElevenLabs API key')
    if cfg['voice_stt_provider'] == 'elevenlabs' and cfg['voice_stt_model'] not in ('scribe_v1', 'scribe_v2', 'scribe_v2_realtime'):
        raise HTTPException(400, 'Choose a supported Scribe model')
    if complete and cfg['voice_tts_provider'] == 'elevenlabs' and (not cfg['voice_id'] or not cfg['voice_tts_model']):
        raise HTTPException(400, 'Choose a voice and a speech model')


def merge(cfg, body):
    unknown = set(body.values) - set(KEYS)
    if unknown:
        raise HTTPException(400, 'Unknown speech setting')
    cfg = dict(cfg)
    for key, value in body.values.items():
        if key in SECRET_FIELDS and not value.strip():
            continue  # An empty password input preserves the stored credential.
        cfg[key] = value.strip()
    if body.clear_api_key:
        cfg['voice_api_key'] = ''
    if body.clear_service_api_key:
        cfg['voice_service_api_key'] = ''
    if cfg['voice_stt_provider'] == 'elevenlabs' and cfg['voice_stt_model'] == 'scribe_v1':
        cfg['voice_stt_model'] = 'scribe_v2'
    if cfg['voice_tts_provider'] == 'service' and urlparse(cfg['voice_tts_url']).path in ('', '/'):
        cfg['voice_tts_url'] = cfg['voice_tts_url'].rstrip('/') + '/synthesize'
    return cfg


def base_url():
    return os.getenv('ELEVENLABS_BASE_URL', 'https://api.elevenlabs.io/v1').rstrip('/')


async def request(cfg, method, path, **kwargs):
    try:
        async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=30)) as session:
            async with session.request(method, base_url() + path,
                                       headers={'xi-api-key': cfg['voice_api_key']}, **kwargs) as response:
                if response.status >= 400:
                    # Remote bodies may echo credentials or private text.
                    status = response.status
                    if status in (401, 403):
                        if method == 'GET' and path == '/models':
                            try:
                                detail = json.loads(await response.content.read(8192)).get('detail', {})
                            except (ValueError, AttributeError):
                                detail = {}
                            if isinstance(detail, dict) and detail.get('status') == 'missing_permissions':
                                raise HTTPException(502, MODELS_READ_REQUIRED)
                        raise HTTPException(502, 'ElevenLabs rejected the API key or its permissions')
                    raise HTTPException(502, f'ElevenLabs returned HTTP {status}')
                if response.content_type == 'application/json':
                    return await response.json()
                return await response.read()
    except HTTPException:
        raise
    except (aiohttp.ClientError, asyncio.TimeoutError, ValueError):
        raise HTTPException(502, 'Could not reach ElevenLabs; check the provider and try again') from None


async def catalogue(cfg):
    models, voices = await asyncio.gather(request(cfg, 'GET', '/models'), request(cfg, 'GET', '/voices'), return_exceptions=True)
    if isinstance(voices, Exception):
        raise voices
    notice = ''
    if isinstance(models, HTTPException) and models.detail == MODELS_READ_REQUIRED:
        notice = MODELS_READ_REQUIRED
        models = [{'model_id': cfg['voice_tts_model'], 'can_do_text_to_speech': True}]
    elif isinstance(models, Exception):
        raise models
    return {
        'notice': notice,
        'models': [{'id': m['model_id'], 'name': m.get('name', m['model_id'])}
                   for m in models if m.get('can_do_text_to_speech')],
        'voices': [{'id': v['voice_id'], 'name': v.get('name', v['voice_id'])}
                   for v in voices.get('voices', [])],
    }


async def list_voices(cfg):
    data = await request(cfg, 'GET', '/voices')
    voices = data.get('voices', [])
    label = lambda v: f"{v.get('name', 'Voice')} [{v['voice_id']}]"
    return {'voices': [label(v) for v in voices],
            'current': next((label(v) for v in voices if v['voice_id'] == cfg['voice_id']), '')}


async def preview(cfg, text):
    if cfg['voice_tts_provider'] == 'openai':
        payload = {'input': text[:200], 'model': cfg['voice_service_tts_model'], 'response_format':'wav'}
        if cfg['voice_service_voice']:
            payload['voice'] = cfg['voice_service_voice']
        audio = await external_request(cfg, 'POST', 'tts', '/audio/speech', json=payload)
        if not isinstance(audio, bytes) or len(audio) < 12 or audio[:4] != b'RIFF' or audio[8:12] != b'WAVE':
            raise HTTPException(502, 'Speech service did not return WAV audio')
        return audio
    payload = {'text': text[:200], 'model_id': cfg['voice_tts_model']}
    if cfg.get('voice_stability'):
        payload['voice_settings'] = {'stability': float(cfg['voice_stability'])}
    if cfg['voice_language']:
        payload['language_code'] = cfg['voice_language']
    pcm = await request(cfg, 'POST', '/text-to-speech/' + quote(cfg['voice_id'], safe='') +
                        '/stream?output_format=pcm_8000', json=payload)
    if not isinstance(pcm, bytes) or len(pcm) % 2 or not pcm:
        raise HTTPException(502, 'Speech provider returned invalid audio')
    buffer = io.BytesIO()
    with wave.open(buffer, 'wb') as wav:
        wav.setnchannels(1)
        wav.setsampwidth(2)
        wav.setframerate(8000)
        wav.writeframes(pcm)
    return buffer.getvalue()


async def external_request(cfg, method, kind, path, **kwargs):
    headers = {}
    if cfg['voice_service_api_key']:
        headers['Authorization'] = 'Bearer ' + cfg['voice_service_api_key']
    try:
        async with aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=30)) as session:
            async with session.request(method, cfg[f'voice_{kind}_url'].rstrip('/') + path, headers=headers, **kwargs) as response:
                if response.status >= 400:
                    raise HTTPException(502, f'Speech service returned HTTP {response.status}; check its URL, model and voice')
                if response.content_type == 'application/json':
                    return await response.json()
                data = await response.read()
                return data
    except (aiohttp.ClientError, asyncio.TimeoutError, ValueError):
        raise HTTPException(502, 'Could not reach the speech service') from None


def install(app, pool_getter):
    @app.get('/api/voice')
    async def get_voice():
        return await settings(pool_getter())

    @app.put('/api/voice')
    async def put_voice(body: VoiceUpdate):
        pool = pool_getter()
        async with pool.acquire() as conn:
            async with conn.transaction():
                await conn.execute('SELECT pg_advisory_xact_lock(719210)')
                cfg = merge(await settings(conn, reveal=True), body)
                validate(cfg)
                for key in KEYS:
                    value = secret_store.encrypt(cfg[key], key) if key in SECRET_FIELDS else cfg[key]
                    await conn.execute('''INSERT INTO config(key,value,updated_at) VALUES($1,$2,NOW())
                        ON CONFLICT(key) DO UPDATE SET value=$2,updated_at=NOW()''', key, value)
        return await settings(pool)

    @app.post('/api/voice/connect')
    async def connect_voice(body: VoiceUpdate):
        cfg = merge(await settings(pool_getter(), reveal=True), body)
        validate(cfg, complete=False)
        if not cfg['voice_api_key']:
            raise HTTPException(400, 'Enter your ElevenLabs API key')
        return await catalogue(cfg)

    @app.get('/api/voice/editor')
    async def voice_editor():
        return Response((Path(__file__).parent / 'static/voice.html').read_text(), media_type='text/html')

    @app.get('/api/voice/style')
    async def voice_style():
        return Response((Path(__file__).parent / 'static/app.css').read_text(), media_type='text/css')


async def convert_audio(data):
    """Decode browser audio locally; direct providers need no conversion service."""
    if len(data) > 20 << 20:
        raise HTTPException(413, 'Audio upload is too large (maximum 20 MB)')
    try:
        process = await asyncio.create_subprocess_exec(
            'ffmpeg', '-hide_banner', '-loglevel', 'error', '-protocol_whitelist', 'pipe', '-i', 'pipe:0',
            '-t', '120', '-ar', '16000', '-ac', '1', '-f', 'wav', 'pipe:1',
            stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL)
    except OSError:
        raise HTTPException(503, 'Audio conversion is unavailable; rebuild the configuration service') from None
    try:
        output, _ = await asyncio.wait_for(process.communicate(data), timeout=20)
        if process.returncode or not output:
            raise HTTPException(400, 'Could not decode this audio recording')
        return output
    finally:
        if process.returncode is None:
            process.kill()
            await process.wait()
