"""Exercise browser and API authentication without a database or live agents."""
import base64
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

import httpx

sys.path.insert(0, str(Path(__file__).resolve().parent))
from test_regressions import load_app
import browser_auth


class BrowserAuthTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.module = load_app()
        self.module.UI_USER = 'admin'
        self.module.UI_PASSWORD = 'fixture-password-é'
        self.module.AGENT_API_KEY = 'fixture-agent-key'
        self.module.PRISM_URL = ''
        # Only these fixture handlers are exercised: no DB, models or calls.
        @self.module.app.api_route('/api/auth-fixture', methods=['GET', 'POST'])
        async def fixture():
            return {'ok': True}
        for route in self.module.app.routes:
            if getattr(route, 'path', '') == '/api/agent-chat':
                self.module.app.routes.remove(route)
                break
        @self.module.app.post('/api/agent-chat')
        async def agent_fixture():
            return {'agent': True}
        self.client = httpx.AsyncClient(transport=httpx.ASGITransport(app=self.module.app),
                                        base_url='https://vox.example.test')
        self.addAsyncCleanup(self.client.aclose)

    async def login(self, **kwargs):
        return await self.client.post('/auth/login', data={
            'username':'admin', 'password':'fixture-password-é'},
            headers={'Origin':'https://vox.example.test', 'Accept':'application/json'}, **kwargs)

    async def test_page_assets_and_protected_documents(self):
        response = await self.client.get('/', headers={'Accept':'text/html'})
        self.assertEqual(response.status_code, 303)
        self.assertEqual(response.headers['location'], '/login')
        self.assertNotIn('www-authenticate', response.headers)
        for path in ('/login', '/static/login.css', '/static/login.js', '/static/app.css', '/static/logo.svg'):
            self.assertEqual((await self.client.get(path)).status_code, 200, path)
        response = await self.client.get('/login')
        self.assertIn('autocomplete="current-password"', response.text)
        self.assertNotIn('fixture-password', response.text)
        self.assertIn("frame-ancestors 'none'", response.headers['content-security-policy'])
        for path in ('/api/auth-fixture', '/static/index.html', '/docs', '/openapi.json'):
            self.assertEqual((await self.client.get(path)).status_code, 401, path)
        response = await self.client.get('/api/auth-fixture')
        self.assertEqual(response.headers['www-authenticate'], 'Basic realm="PrismVox"')
        response = await self.client.get('/api/auth-fixture', headers={'Sec-Fetch-Mode':'cors', 'Accept':'application/json'})
        self.assertEqual(response.status_code, 401)
        self.assertNotIn('www-authenticate', response.headers)

    async def test_login_cookie_csrf_logout_and_api_compatibility(self):
        response = await self.login()
        self.assertEqual(response.status_code, 200, response.text)
        cookie = response.headers['set-cookie']
        for attribute in ('HttpOnly', 'Secure', 'SameSite=strict', 'Max-Age=28800'):
            self.assertIn(attribute, cookie)
        self.assertNotIn('fixture-password', cookie)
        self.assertEqual((await self.client.get('/api/auth-fixture')).status_code, 200)
        response = await self.client.get('/')
        self.assertIn('action="/auth/logout"', response.text)
        self.assertEqual(response.headers['cache-control'], 'no-store')
        self.assertEqual((await self.client.get('/login')).status_code, 303)
        for headers in ({}, {'Origin':'https://evil.example'}, {'Origin':'null'}):
            self.assertEqual((await self.client.post('/api/auth-fixture', headers=headers)).status_code, 403)
        origin = {'Origin':'https://vox.example.test'}
        self.assertEqual((await self.client.post('/api/auth-fixture', headers=origin)).status_code, 200)
        self.assertEqual((await self.client.post('/api/agent-chat', headers=origin)).status_code, 200)
        response = await self.client.post('/auth/logout', headers=origin)
        self.assertEqual(response.status_code, 303)
        self.assertIn('Max-Age=0', response.headers['set-cookie'])
        self.assertEqual((await self.client.get('/api/auth-fixture')).status_code, 401)
        basic = base64.b64encode('admin:fixture-password-é'.encode()).decode()
        self.assertEqual((await self.client.post('/api/auth-fixture', headers={'Authorization':'Basic '+basic})).status_code, 200)
        bearer = {'Authorization':'Bearer fixture-agent-key'}
        self.assertEqual((await self.client.post('/api/agent-chat', headers=bearer)).status_code, 200)
        self.assertEqual((await self.client.get('/api/auth-fixture', headers=bearer)).status_code, 401)

    async def test_invalid_expired_and_previous_credentials_sessions(self):
        await self.login()
        token = self.client.cookies.get(browser_auth.COOKIE)
        for bad in (token[:-1] + ('0' if token[-1] != '0' else '1'), 'not-a-session', '1.a.b', 'x'*200):
            self.client.cookies.clear()
            self.client.cookies.set(browser_auth.COOKIE, bad)
            self.assertEqual((await self.client.get('/api/auth-fixture')).status_code, 401)
        self.client.cookies.clear()
        self.client.cookies.set(browser_auth.COOKIE, token)
        with patch.object(browser_auth.time, 'time', return_value=int(token.split('.')[0]) + 1):
            self.assertEqual((await self.client.get('/api/auth-fixture')).status_code, 401)
        self.module.UI_PASSWORD = 'changed-password'
        self.assertEqual((await self.client.get('/api/auth-fixture')).status_code, 401)
        self.module.UI_PASSWORD = ''
        self.assertEqual((await self.client.get('/login')).status_code, 303)

    async def test_failures_throttling_and_cross_origin_login(self):
        response = await self.client.post('/auth/login', data={'username':'admin', 'password':'wrong'},
                                           headers={'Origin':'https://evil.example', 'Accept':'application/json'})
        self.assertEqual(response.status_code, 403)
        for _ in range(10):
            response = await self.client.post('/auth/login', data={'username':'admin', 'password':'wrong'},
                                              headers={'Accept':'application/json'})
            self.assertEqual(response.status_code, 401)
            self.assertNotIn('set-cookie', response.headers)
        response = await self.login()
        self.assertEqual(response.status_code, 429)
        self.assertIn('retry-after', response.headers)

    async def test_native_form_and_escaped_error_page(self):
        response = await self.client.post('/auth/login', data={'username':'"><script>alert(1)</script>', 'password':'wrong'})
        self.assertEqual(response.status_code, 401)
        self.assertNotIn('<script>alert(1)</script>', response.text)
        self.assertIn('data-error="credentials"', response.text)
        response = await self.client.post('/auth/login', data={'username':'admin', 'password':'fixture-password-é'})
        self.assertEqual(response.status_code, 303)
        self.assertEqual(response.headers['location'], '/')

    async def test_https_proxy_and_plain_http(self):
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=self.module.app),
                                     base_url='http://vox.example.test') as client:
            data = {'username':'admin', 'password':'fixture-password-é'}
            for origin, secure in [('https://vox.example.test', True), ('http://vox.example.test', False)]:
                response = await client.post('/auth/login', data=data,
                                             headers={'Origin':origin, 'Accept':'application/json'})
                self.assertEqual(response.status_code, 200)
                self.assertEqual('Secure' in response.headers['set-cookie'], secure)


if __name__ == '__main__':
    unittest.main()
