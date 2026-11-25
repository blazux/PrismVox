"""API redirects must preserve the configured origin and private request body."""
import sys
from pathlib import Path
import unittest
from unittest.mock import AsyncMock, patch

from aiohttp import web, ClientError

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_regressions import load_app


class ModelRedirectTests(unittest.IsolatedAsyncioTestCase):
    async def test_model_cross_origin_redirect_is_refused_and_local_path_redirect_works(self):
        leaked = []
        async def target(request):
            leaked.append(await request.read())
            return web.json_response({'choices':[{'message':{'content':'Synthetic reply'}}]})
        foreign = web.Application(); foreign.router.add_post('/capture', target)
        foreign_runner = web.AppRunner(foreign); await foreign_runner.setup()
        self.addAsyncCleanup(foreign_runner.cleanup)
        foreign_site = web.TCPSite(foreign_runner, '127.0.0.1', 0); await foreign_site.start()
        destination = 'http://127.0.0.1:' + str(foreign_site._server.sockets[0].getsockname()[1]) + '/capture'
        mode = {'local':False, 'code':307}
        async def source(request):
            if request.path == '/canonical':
                self.assertEqual(request.headers.get('Authorization'), 'Bearer fixture-key')
                self.assertEqual((await request.json())['messages'][0]['content'], 'Private fixture')
                return web.json_response({'choices':[{'message':{'content':'Synthetic reply'}}]})
            return web.Response(status=mode['code'], headers={'Location':'/canonical' if mode['local'] else destination})
        origin = web.Application(); origin.router.add_post('/{path:.*}', source)
        runner = web.AppRunner(origin); await runner.setup(); self.addAsyncCleanup(runner.cleanup)
        site = web.TCPSite(runner, '127.0.0.1', 0); await site.start()
        url = 'http://127.0.0.1:' + str(site._server.sockets[0].getsockname()[1])
        app = load_app()
        with patch.object(app, '_backend_for', AsyncMock(return_value=(url, {'Authorization':'Bearer fixture-key'}))), \
                patch.object(app, '_model_for', AsyncMock(return_value='fixture-model')):
            for code in (307, 308):
                mode.update(local=False, code=code)
                with self.assertRaises(ClientError):
                    await app._supervision_complete(None, [{'role':'user','content':'Private fixture'}])
                self.assertEqual(leaked, [])
                mode['local'] = True
                result = await app._supervision_complete(None, [{'role':'user','content':'Private fixture'}])
                self.assertEqual(result['content'], 'Synthetic reply')
