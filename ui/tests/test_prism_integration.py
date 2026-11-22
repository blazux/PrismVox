"""Exercise saved connections, synthetic handshakes and non-mutating previews."""
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
import uuid
from unittest.mock import AsyncMock, patch

import asyncpg
from aiohttp import web
import httpx

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_regressions import load_app
import prism_integration
import secret_store


class ConnectionValidationTests(unittest.TestCase):
    def test_disable_keeps_settings_and_blank_token_is_not_erasure(self):
        cfg = {'enabled': True, 'prism_url': 'http://prism.invalid/prefix', 'prism_token': 'fixture-token',
               'prism_public_url': 'https://prism.example.test', 'model': 'source::model'}
        changed = prism_integration.merge(cfg, prism_integration.ConnectionUpdate(enabled=False))
        self.assertFalse(changed['enabled'])
        self.assertEqual(changed['prism_token'], 'fixture-token')
        self.assertEqual(changed['model'], 'source::model')
        cleared = prism_integration.merge(changed, prism_integration.ConnectionUpdate(clear_prism_token=True))
        self.assertEqual(cleared['prism_token'], '')
        with self.assertRaises(Exception):
            prism_integration.merge(cfg, prism_integration.ConnectionUpdate(prism_url='https://other-prism.invalid'))
        moved_disabled = prism_integration.merge(cfg, prism_integration.ConnectionUpdate(enabled=False, prism_url='https://other-prism.invalid'))
        self.assertEqual(moved_disabled['prism_token'], '')
        for url in ('https://user:secret@prism.invalid', 'file:///etc/passwd', 'http://prism.invalid/?key=secret'):
            with self.assertRaises(Exception):
                prism_integration.merge(cfg, prism_integration.ConnectionUpdate(prism_url=url))


class ConnectionProbeTests(unittest.IsolatedAsyncioTestCase):
    async def test_capabilities_and_auth_failure_without_generation(self):
        seen = []
        async def handler(request):
            seen.append((request.path, request.headers.get('Authorization')))
            if request.headers.get('Authorization') != 'Bearer fixture-token':
                return web.json_response({'detail': 'provider-private-token'}, status=401)
            return web.json_response({'ok': True, 'service': 'prism', 'voice': True, 'inference': True})
        app = web.Application(); app.router.add_get('/prefix/api/voice/capabilities', handler)
        runner = web.AppRunner(app); await runner.setup()
        site = web.TCPSite(runner, '127.0.0.1', 0); await site.start()
        self.addAsyncCleanup(runner.cleanup)
        cfg = {'enabled': True, 'prism_url': 'http://127.0.0.1:' + str(site._server.sockets[0].getsockname()[1]) + '/prefix', 'prism_token': 'fixture-token'}
        self.assertTrue((await prism_integration.status(cfg))['connected'])
        cfg['prism_token'] = 'wrong'
        state = await prism_integration.status(cfg)
        self.assertFalse(state['connected']); self.assertEqual(state['status'], 'unauthorized')
        self.assertNotIn('provider-private-token', json.dumps(state))
        self.assertTrue(all(path.endswith('/capabilities') for path, token in seen))

    async def test_model_probe_uses_authenticated_models_and_handles_optional_listing(self):
        module = load_app(); seen = []
        async def handler(request):
            seen.append(request.path)
            self.assertEqual(request.headers.get('Authorization'), 'Bearer fixture-token')
            if request.path.startswith('/missing'):
                return web.Response(status=404)
            return web.json_response({'data': [{'id': 'fixture-model'}]})
        app = web.Application(); app.router.add_route('*', '/{path:.*}', handler)
        runner = web.AppRunner(app); await runner.setup()
        site = web.TCPSite(runner, '127.0.0.1', 0); await site.start()
        self.addAsyncCleanup(runner.cleanup)
        url = 'http://127.0.0.1:' + str(site._server.sockets[0].getsockname()[1])
        headers = {'Authorization': 'Bearer fixture-token'}
        self.assertEqual(await module._model_health(url + '/v1', headers), 'ok')
        self.assertEqual(await module._model_health(url + '/missing', headers), 'configured')
        self.assertEqual(seen, ['/v1/models', '/missing/models'])

    async def test_preview_overrides_voice_without_saving(self):
        module = load_app()
        cfg = {'voice_tts_provider': 'elevenlabs', 'voice_id': 'saved-voice'}
        async def synth(settings, text):
            self.assertEqual(settings['voice_id'], 'draft-voice')
            return b'RIFFfixture'
        with patch.object(module.voice_config, 'settings', AsyncMock(return_value=dict(cfg))), \
             patch.object(module.voice_config, 'request', AsyncMock(return_value={'voices': [{'voice_id': 'draft-voice', 'name': 'Draft'}]})), \
             patch.object(module.voice_config, 'preview', side_effect=synth), \
             patch.object(module, '_pool_or_error', return_value=object()), \
             patch.object(module, 'tts_set_voice', AsyncMock(side_effect=AssertionError('Preview must not save'))):
            response = await module.tts_preview(module.TTSPreview(text='Synthetic preview', voice='Draft [draft-voice]'))
        self.assertEqual(response.body, b'RIFFfixture')
        self.assertEqual(cfg['voice_id'], 'saved-voice')


@unittest.skipUnless(os.getenv('VOX_TEST_DATABASE_URL'), 'requires disposable PostgreSQL')
class SavedConnectionTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.env = patch.dict(os.environ, {'SECRET_KEY_PATH': self.temp.name + '/key',
            'PRISM_URL': 'http://bootstrap.invalid', 'PRISM_TOKEN': 'bootstrap-fixture-token'})
        self.env.start()
        self.admin = await asyncpg.connect(os.environ['VOX_TEST_DATABASE_URL'])
        self.schema = 'prism_link_' + uuid.uuid4().hex
        await self.admin.execute(f'CREATE SCHEMA {self.schema}')
        self.pool = await asyncpg.create_pool(os.environ['VOX_TEST_DATABASE_URL'], server_settings={'search_path': self.schema + ',public'})
        await self.pool.execute('CREATE TABLE config(key text primary key,value text not null,updated_at timestamptz default now());CREATE TABLE mcp_servers(id serial primary key,auth_token text)')
        await secret_store.migrate(self.pool); await prism_integration.migrate(self.pool)
        self.module = load_app(); self.module._pool = self.pool; self.module.UI_PASSWORD = 'fixture-admin'
        self.client = httpx.AsyncClient(transport=httpx.ASGITransport(app=self.module.app), base_url='http://vox.invalid', auth=('admin', 'fixture-admin'))

    async def asyncTearDown(self):
        await self.client.aclose(); await self.pool.close()
        await self.admin.execute(f'DROP SCHEMA {self.schema} CASCADE'); await self.admin.close()
        self.env.stop(); self.temp.cleanup()

    async def test_bootstrap_save_reload_secret_preservation_and_disable(self):
        response = await self.client.get('/api/prism/connection')
        self.assertEqual(response.status_code, 200); self.assertTrue(response.json()['token_configured'])
        self.assertNotIn('bootstrap-fixture-token', response.text)
        self.assertTrue((await self.pool.fetchval("SELECT value FROM config WHERE key='prism_token'")).startswith(secret_store.PREFIX))
        rejected = await self.client.put('/api/prism/connection', json={'prism_url': 'https://saved.invalid/prefix', 'prism_token': ''})
        self.assertEqual(rejected.status_code, 400)
        self.assertEqual((await prism_integration.settings(self.pool))['prism_url'], 'http://bootstrap.invalid')
        saved = await self.client.put('/api/prism/connection', json={'prism_url': 'https://saved.invalid/prefix', 'model': 'source::model', 'prism_token': 'new-service-fixture'})
        self.assertEqual(saved.status_code, 200, saved.text)
        await prism_integration.migrate(self.pool)
        cfg = await prism_integration.settings(self.pool, reveal=True)
        self.assertEqual(cfg['prism_url'], 'https://saved.invalid/prefix')
        self.assertEqual(cfg['prism_token'], 'new-service-fixture')
        self.assertEqual((await self.client.put('/api/prism/connection', json={'prism_token': ''})).status_code, 200)
        endpoint, headers, model = await prism_integration.inference(self.pool)
        self.assertEqual(endpoint, 'https://saved.invalid/prefix/api/voice/inference/v1')
        self.assertEqual(headers, {'Authorization': 'Bearer new-service-fixture'}); self.assertEqual(model, 'source::model')
        local = await self.client.put('/api/prism/connection', json={'model': ''})
        self.assertEqual(local.status_code, 200)
        self.assertIsNone(await prism_integration.inference(self.pool))
        self.assertEqual(await self.module._backend_for(self.pool), (self.module.LLM_BASE_URL, self.module._llm_headers()))
        self.assertEqual((await self.client.put('/api/prism/connection', json={'model': 'default'})).status_code, 200)
        self.assertEqual((await prism_integration.inference(self.pool))[2], 'default')
        generic = await self.client.put('/api/config', json={'values': {'prism_token': 'bad', 'prism_url': 'http://wrong.invalid'}})
        self.assertEqual(generic.status_code, 200)
        self.assertEqual((await prism_integration.settings(self.pool))['prism_url'], 'https://saved.invalid/prefix')
        disabled = await self.client.put('/api/prism/connection', json={'enabled': False})
        self.assertEqual(disabled.status_code, 200)
        self.assertIsNone(await prism_integration.inference(self.pool))
        self.assertEqual((await prism_integration.settings(self.pool, reveal=True))['prism_token'], 'new-service-fixture')
        self.assertEqual((await self.client.put('/api/prism/connection', json={'clear_prism_token': True})).status_code, 200)
        self.assertFalse((await self.client.get('/api/prism/connection')).json()['token_configured'])
        self.assertEqual((await self.client.put('/api/prism/connection', json={'enabled': True})).status_code, 400)

    async def test_legacy_ui_url_without_token_does_not_erase_agent_bootstrap(self):
        await self.pool.execute('DELETE FROM config')
        with patch.dict(os.environ, {'PRISM_TOKEN': '', 'CORTEX_TOKEN': ''}):
            await prism_integration.migrate(self.pool)
            stored = dict(await self.pool.fetch('SELECT key,value FROM config'))
            self.assertEqual(stored['prism_url'], 'http://bootstrap.invalid')
            self.assertEqual(stored['prism_enabled'], 'true')
            self.assertNotIn('prism_token', stored)
            cfg = await prism_integration.settings(self.pool, reveal=True)
            self.assertEqual((await prism_integration.status(cfg))['status'], 'incomplete')
        # Restarting the UI must not attach an environment credential to a
        # previously saved URL, even when that bootstrap lacked a token.
        with patch.dict(os.environ, {'PRISM_URL': 'http://different.invalid', 'PRISM_TOKEN': 'different-service-fixture'}):
            await prism_integration.migrate(self.pool)
        self.assertIsNone(await self.pool.fetchval("SELECT value FROM config WHERE key='prism_token'"))
        self.assertEqual(await self.pool.fetchval("SELECT value FROM config WHERE key='prism_url'"), 'http://bootstrap.invalid')

    async def test_ui_without_prism_environment_does_not_disable_agent_connection(self):
        await self.pool.execute('DELETE FROM config')
        with patch.dict(os.environ, {'PRISM_URL': '', 'PRISM_TOKEN': '', 'CORTEX_URL': '', 'CORTEX_TOKEN': ''}):
            await prism_integration.migrate(self.pool)
            self.assertEqual(await self.pool.fetchval('SELECT COUNT(*) FROM config'), 0)
            self.assertFalse((await prism_integration.settings(self.pool))['enabled'])

    async def test_management_routes_require_admin_credential(self):
        for path in ('/api/prism/connection', '/api/prism/connection/status', '/console'):
            response = await self.client.get(path, auth=None, headers={'Authorization': 'Bearer fixture-agent'})
            self.assertEqual(response.status_code, 401, path)


if __name__ == '__main__':
    unittest.main()
