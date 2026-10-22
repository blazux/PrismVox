"""Voice configuration and provider fixtures; never contact a real provider."""
import asyncio
import base64
import importlib.util
import io
import os
import shutil
from pathlib import Path
import sys
import tempfile
import unittest
import uuid
import wave
from unittest.mock import patch, AsyncMock

import asyncpg
from aiohttp import web
import httpx

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_regressions import load_app
import secret_store
import voice_config


@unittest.skipUnless(os.getenv('VOX_TEST_DATABASE_URL'), 'requires disposable PostgreSQL')
class VoiceSettingsTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.previous = {key: os.environ.get(key) for key in ('SECRET_KEY_PATH', 'ELEVENLABS_BASE_URL')}
        os.environ['SECRET_KEY_PATH'] = self.temp.name + '/key'
        dsn = os.environ['VOX_TEST_DATABASE_URL']
        self.schema = 'voice_' + uuid.uuid4().hex
        self.admin = await asyncpg.connect(dsn)
        await self.admin.execute(f'CREATE SCHEMA {self.schema}')
        self.pool = await asyncpg.create_pool(dsn, min_size=1, max_size=3,
            server_settings={'search_path': self.schema + ',public'})
        await self.pool.execute('''CREATE TABLE config(key text primary key,value text not null,updated_at timestamptz default now());
            CREATE TABLE mcp_servers(id serial primary key,auth_token text)''')
        await secret_store.migrate(self.pool)
        self.app = load_app()
        self.app._pool = self.pool
        self.app.UI_PASSWORD = 'fixture-admin'
        self.app.AGENT_API_KEY = 'fixture-agent'
        self.requests = []
        self.models_denied = False
        async def provider(request):
            self.requests.append((request.method, request.path, request.headers.get('xi-api-key')))
            if request.headers.get('xi-api-key') != 'fixture-private-key':
                return web.json_response({'error': 'fixture-private-key should never reach the UI'}, status=401)
            if request.path == '/models':
                if self.models_denied:
                    return web.json_response({'detail':{'status':'missing_permissions','message':'fixture-private-key'}},status=401)
                return web.json_response([{'model_id':'fixture-tts','name':'Fixture speech','can_do_text_to_speech':True},
                                          {'model_id':'not-tts','can_do_text_to_speech':False}])
            if request.path == '/voices':
                return web.json_response({'voices':[{'voice_id':'fixture-voice','name':'Test voice'}]})
            return web.Response(body=b'\x00\x00'*800, content_type='audio/pcm')
        remote = web.Application()
        remote.router.add_route('*','/{path:.*}',provider)
        self.runner = web.AppRunner(remote)
        await self.runner.setup()
        site = web.TCPSite(self.runner,'127.0.0.1',0)
        await site.start()
        os.environ['ELEVENLABS_BASE_URL'] = 'http://127.0.0.1:' + str(site._server.sockets[0].getsockname()[1])
        self.client = httpx.AsyncClient(transport=httpx.ASGITransport(app=self.app.app), base_url='http://fixture.invalid',
                                      auth=('admin','fixture-admin'))

    async def asyncTearDown(self):
        await self.client.aclose()
        await self.runner.cleanup()
        await self.pool.close()
        await self.admin.execute(f'DROP SCHEMA {self.schema} CASCADE')
        await self.admin.close()
        for key,value in self.previous.items():
            if value is None: os.environ.pop(key,None)
            else: os.environ[key]=value
        self.temp.cleanup()

    def payload(self):
        return {'values':{'voice_stt_provider':'elevenlabs','voice_tts_provider':'elevenlabs',
            'voice_api_key':'fixture-private-key','voice_stt_model':'scribe_v2_realtime',
            'voice_tts_model':'fixture-tts','voice_id':'fixture-voice','voice_language':'fr','voice_stability':'0.65'}}

    async def test_connect_does_not_save_then_save_is_encrypted_and_cross_process(self):
        response = await self.client.post('/api/voice/connect',json=self.payload())
        self.assertEqual(response.status_code,200,response.text)
        self.assertEqual(response.json()['models'],[{'id':'fixture-tts','name':'Fixture speech'}])
        self.assertEqual(await self.pool.fetchval('SELECT count(*) FROM config'),0)
        response = await self.client.put('/api/voice',json=self.payload())
        self.assertEqual(response.status_code,200,response.text)
        self.assertNotIn('fixture-private-key',response.text)
        self.assertNotIn('enc:v1:',response.text)
        self.assertTrue(response.json()['api_key_configured'])
        self.assertEqual(response.json()['voice_stability'], '0.65')
        value = await self.pool.fetchval("SELECT value FROM config WHERE key='voice_api_key'")
        self.assertTrue(value.startswith(secret_store.PREFIX))
        self.assertEqual(secret_store.decrypt(value,'voice_api_key'),'fixture-private-key')
        other = load_app(); other._pool=self.pool;other.UI_PASSWORD='fixture-admin'
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=other.app),base_url='http://fixture.invalid',
                                     auth=('admin','fixture-admin')) as second:
            self.assertEqual((await second.get('/api/voice')).json()['voice_id'],'fixture-voice')
        self.assertNotIn('fixture-private-key',(await self.client.get('/api/config')).text)
        self.assertNotIn('enc:v1:',(await self.client.get('/api/config')).text)
        blank = self.payload();blank['values']['voice_api_key']=''
        self.assertEqual((await self.client.put('/api/voice',json=blank)).status_code,200)
        cfg = await voice_config.settings(self.pool,reveal=True)
        self.assertEqual(cfg['voice_api_key'],'fixture-private-key')
        # Generic settings round trips cannot change providers or wipe their key.
        await self.client.put('/api/config',json={'values':{'voice_api_key':'bad','voice_id':'bad'}})
        self.assertEqual((await self.client.get('/api/voice')).json()['voice_id'],'fixture-voice')
        voices = await self.client.get('/api/tts/voices')
        self.assertEqual(voices.json()['current'],'Test voice [fixture-voice]')
        preview = await self.client.post('/api/tts/preview',json={'text':'Synthetic preview'})
        self.assertEqual(preview.status_code,200,preview.text if preview.status_code!=200 else '')
        with wave.open(io.BytesIO(preview.content)) as wav:
            self.assertEqual(wav.getframerate(),8000)
            self.assertEqual(wav.getnchannels(),1)

    async def test_models_read_permission_does_not_block_voice_selection(self):
        self.models_denied=True
        response=await self.client.post('/api/voice/connect',json=self.payload())
        self.assertEqual(response.status_code,200,response.text)
        self.assertEqual(response.json()['models'],[{'id':'fixture-tts','name':'fixture-tts'}])
        self.assertEqual(response.json()['voices'],[{'id':'fixture-voice','name':'Test voice'}])
        self.assertIn('Models Read',response.json()['notice'])
        self.assertNotIn('fixture-private-key',response.text)
        self.assertEqual(await self.pool.fetchval('SELECT count(*) FROM config'),0)

    async def test_validation_auth_and_secret_migration(self):
        await self.pool.execute("INSERT INTO config(key,value) VALUES('voice_api_key','fixture-private-key')")
        await secret_store.migrate(self.pool)
        encrypted=await self.pool.fetchval("SELECT value FROM config WHERE key='voice_api_key'")
        self.assertTrue(encrypted.startswith(secret_store.PREFIX))
        response=await self.client.put('/api/voice',json={'values':{'voice_stt_provider':'unknown'}})
        self.assertEqual(response.status_code,400)
        response=await self.client.put('/api/voice',json={'values':{'voice_tts_url':'http://user:password@fixture.invalid'}})
        self.assertEqual(response.status_code,400)
        for path in ('/api/voice','/api/voice/editor','/api/voice/style'):
            r=await self.client.get(path,auth=None,headers={'Authorization':'Bearer fixture-agent'})
            self.assertEqual(r.status_code,401)
        legacy=self.payload();legacy['values']['voice_stt_model']='scribe_v1'
        upgraded=await self.client.put('/api/voice',json=legacy)
        self.assertEqual(upgraded.json()['voice_stt_model'],'scribe_v2')
        bad=self.payload();bad['values']['voice_api_key']='wrong'
        response=await self.client.post('/api/voice/connect',json=bad)
        self.assertEqual(response.status_code,502)
        self.assertNotIn('fixture-private-key',response.text)
        # Removing the persistent key must never silently create a replacement.
        secret_store.key_path().unlink()
        with self.assertRaises(RuntimeError):await secret_store.migrate(self.pool)
        self.assertFalse(secret_store.key_path().exists())


    async def test_saved_http_service_is_used_for_voice_management_and_conversion(self):
        seen=[]
        async def service(request):
            seen.append(request.path)
            if request.path=='/health':return web.json_response({'status':'ok','engine':'melo','clone_enabled':False})
            if request.path=='/voices':return web.json_response({'voices':['fixture.wav'],'current':'fixture.wav'})
            return web.Response(body=b'RIFFfixture',content_type='audio/wav')
        server=web.Application();server.router.add_route('*','/{path:.*}',service)
        runner=web.AppRunner(server);await runner.setup();site=web.TCPSite(runner,'127.0.0.1',0);await site.start()
        url='http://127.0.0.1:'+str(site._server.sockets[0].getsockname()[1])
        try:
            saved=await self.client.put('/api/voice',json={'values':{'voice_stt_provider':'service','voice_tts_provider':'service','voice_tts_url':url}})
            self.assertEqual(saved.status_code,200,saved.text)
            backend=await self.client.get('/api/tts/backend')
            self.assertEqual(backend.json(),{'backend':'melo','clone_enabled':False})
            self.assertEqual((await self.client.get('/api/tts/voices')).json()['current'],'fixture.wav')
            self.assertEqual((await self.client.post('/api/tts/preview',json={'text':'fixture'})).status_code,200)
            upload=await self.client.post('/api/tts/voices',files={'file':('sample.webm',b'fixture','audio/webm')},data={'name':'Test voice'})
            self.assertEqual(upload.status_code,200,upload.text)
            self.assertIn('/convert',seen)
            self.assertIn('/synthesize',seen)
        finally:await runner.cleanup()


    async def test_generic_speech_api_persistence_preview_and_key_isolation(self):
        bodies = []
        sample = io.BytesIO()
        with wave.open(sample, 'wb') as out:
            out.setnchannels(1); out.setsampwidth(2); out.setframerate(16000); out.writeframes(b'\0\0'*1600)
        async def external(request):
            self.assertEqual(request.path, '/v1/audio/speech')
            self.assertEqual(request.headers.get('Authorization'), 'Bearer external-fixture')
            self.assertNotIn('xi-api-key', request.headers)
            bodies.append(await request.json())
            return web.Response(body=sample.getvalue(), content_type='audio/wav')
        app = web.Application(); app.router.add_post('/v1/audio/speech', external)
        runner = web.AppRunner(app); await runner.setup()
        site = web.TCPSite(runner, '127.0.0.1', 0); await site.start()
        base = 'http://127.0.0.1:' + str(site._server.sockets[0].getsockname()[1]) + '/v1'
        try:
            response = await self.client.put('/api/voice', json={'values':{
                'voice_stt_provider':'openai','voice_tts_provider':'openai',
                'voice_stt_url':base,'voice_tts_url':base,
                'voice_service_stt_model':'custom-recognition','voice_service_tts_model':'custom-speech',
                'voice_service_voice':'custom-voice','voice_service_api_key':'external-fixture'}})
            self.assertEqual(response.status_code, 200, response.text)
            self.assertEqual(response.json()['voice_tts_url'], base)
            self.assertTrue(response.json()['service_api_key_configured'])
            self.assertNotIn('external-fixture', response.text)
            value = await self.pool.fetchval("SELECT value FROM config WHERE key='voice_service_api_key'")
            self.assertTrue(value.startswith(secret_store.PREFIX))
            self.assertEqual(secret_store.decrypt(value,'voice_service_api_key'),'external-fixture')
            self.assertEqual((await self.client.get('/api/config')).json()['voice_service_api_key'],'')
            preview = await self.client.post('/api/tts/preview',json={'text':'Bonjour.'})
            self.assertEqual(preview.status_code,200,preview.text if preview.status_code!=200 else '')
            self.assertTrue(preview.content.startswith(b'RIFF'))
            self.assertEqual(bodies,[{'input':'Bonjour.','model':'custom-speech','voice':'custom-voice','response_format':'wav'}])
            self.assertEqual((await self.client.get('/api/tts/backend')).json()['clone_enabled'],False)
        finally:
            await runner.cleanup()


class AudioConversionTests(unittest.IsolatedAsyncioTestCase):
    @unittest.skipUnless(shutil.which('ffmpeg'), 'requires ffmpeg')
    async def test_browser_audio_conversion_uses_local_decoder(self):
        data=io.BytesIO()
        with wave.open(data,'wb') as wav:
            wav.setnchannels(1);wav.setsampwidth(2);wav.setframerate(8000);wav.writeframes(b'\0\0'*800)
        decoded=await voice_config.convert_audio(data.getvalue())
        with wave.open(io.BytesIO(decoded)) as wav:
            self.assertEqual(wav.getframerate(),16000)
            self.assertEqual(wav.getnchannels(),1)
        from fastapi import HTTPException
        with self.assertRaises(HTTPException):await voice_config.convert_audio(b'not audio')



class ModelTierTests(unittest.IsolatedAsyncioTestCase):
    async def test_local_standard_keeps_gateway_credentials_isolated(self):
        app=load_app();app.LLM_BASE_URL='http://gateway.invalid/v1';app.LLM_API_KEY='private-gateway-fixture'
        cfg={'llm_url_standard':'http://fast-llm:8080/v1','llm_model_standard':'fast','llm_model_oncall':'heavy'}
        class Pool:
            async def fetchval(self,sql,key):return cfg.get(key)
        pool=Pool()
        self.assertEqual(await app._backend_for_model(pool,'fast'),('http://fast-llm:8080/v1',{}))
        self.assertEqual(await app._backend_for_model(pool,'heavy'),('http://gateway.invalid/v1',{'Authorization':'Bearer private-gateway-fixture'}))

        cfg['llm_url_standard']='   '
        self.assertEqual(await app._backend_for(pool,'standard'),('http://gateway.invalid/v1',{'Authorization':'Bearer private-gateway-fixture'}))

    async def test_text_stream_and_tool_loop_use_selected_backend(self):
        app=load_app();app.LLM_API_KEY='private-gateway-fixture'
        seen=[]
        async def backend(request):
            payload=await request.json()
            seen.append((payload['model'],request.headers.get('Authorization')))
            if payload.get('stream'):
                return web.Response(text='data: {"choices":[{"delta":{"content":"Fixture response"}}]}\n\ndata: [DONE]\n\n',content_type='text/event-stream')
            return web.json_response({'choices':[{'message':{'content':'Fixture response'}}]})
        server=web.Application();server.router.add_post('/v1/chat/completions',backend)
        runner=web.AppRunner(server);await runner.setup();site=web.TCPSite(runner,'127.0.0.1',0);await site.start()
        url='http://127.0.0.1:'+str(site._server.sockets[0].getsockname()[1])+'/v1'
        cfg={'llm_url_standard':url,'llm_model_standard':'fast','llm_model_oncall':'heavy'}
        class Pool:
            async def fetchval(self,sql,key):return cfg.get(key)
        app._pool=Pool();app.LLM_BASE_URL='http://gateway.invalid/v1'
        try:
            pieces=[piece async for piece in app._sglang_stream([{'role':'user','content':'Synthetic fixture'}],'fast')]
            self.assertEqual(''.join(pieces),'Fixture response')
            self.assertEqual(await app._agent_run_tool_loop([{'role':'user','content':'Synthetic fixture'}],[],{},'fast'),'Fixture response')
            self.assertEqual(seen,[('fast',None),('fast',None)])
        finally:await runner.cleanup()

class VoiceStabilityTests(unittest.IsolatedAsyncioTestCase):
    def config(self):
        return {**{k: default for k, (_, default) in voice_config.DEFAULTS.items()}, 'voice_stt_provider':'service', 'voice_tts_provider':'service'}

    async def test_rejects_invalid_values_and_accepts_zero_and_provider_default(self):
        cfg = self.config()
        for value in ('NaN', 'Inf', '-0.1', '1.1', 'invalid'):
            cfg['voice_stability'] = value
            with self.assertRaises(voice_config.HTTPException):
                voice_config.validate(cfg)
        for value in ('', '0', '0.65', '1'):
            cfg['voice_stability'] = value
            voice_config.validate(cfg)

    async def test_preview_passes_stability_without_changing_other_voice_settings(self):
        cfg = self.config()
        for value in ('', '0', '0.65'):
            cfg['voice_stability'] = value
            with patch.object(voice_config, 'request', new_callable=AsyncMock, return_value=b'\0\0'*800) as remote:
                wav = await voice_config.preview(cfg, 'Bonjour.')
                self.assertTrue(wav.startswith(b'RIFF'))
                payload = remote.call_args.kwargs['json']
                if value:
                    self.assertEqual(payload['voice_settings'], {'stability':float(value)})
                else:
                    self.assertNotIn('voice_settings', payload)
