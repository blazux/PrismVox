"""MCP edits preserve write-only credentials until an explicit replacement/removal."""
import os
import unittest

import httpx
import test_regressions as fixtures
import secret_store


@unittest.skipUnless(os.getenv('VOX_TEST_DATABASE_URL'), 'requires disposable PostgreSQL')
class MCPEditTests(unittest.IsolatedAsyncioTestCase):
    asyncSetUp = fixtures.DatabaseTests.asyncSetUp
    asyncTearDown = fixtures.DatabaseTests.asyncTearDown

    async def test_edit_blank_key_preserves_secret_and_clear_is_explicit(self):
        await secret_store.migrate(self.pool)
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=self.a.app),
                base_url='http://fixture.invalid', auth=('admin', 'fixture-only')) as client:
            created = await client.post('/api/mcp', json={'name': 'Fixture', 'type': 'http',
                'url': 'https://tools.invalid/mcp', 'auth_token': 'private-fixture-token', 'enabled': False})
            self.assertEqual(created.status_code, 201, created.text)
            server = created.json()['id']
            listing = await client.get('/api/mcp')
            self.assertNotIn('private-fixture-token', listing.text)
            self.assertNotIn(secret_store.PREFIX, listing.text)
            self.assertEqual(listing.json()[0]['auth_token'], '')
            self.assertTrue(listing.json()[0]['has_auth_token'])
            self.assertFalse(listing.json()[0]['enabled'])
            saved = await client.put(f'/api/mcp/{server}', json={'name': 'Edited fixture', 'type': 'sse',
                'url': 'https://other-tools.invalid/sse?version=fixture', 'auth_token': '', 'enabled': False})
            self.assertEqual(saved.status_code, 200, saved.text)
            stored = await self.pool.fetchrow('SELECT * FROM mcp_servers WHERE id=$1', server)
            self.assertEqual(secret_store.decrypt(stored['auth_token'], f'mcp:{server}'), 'private-fixture-token')
            self.assertEqual(stored['name'], 'Edited fixture'); self.assertFalse(stored['enabled'])
            invalid = await client.put(f'/api/mcp/{server}', json={'url': 'https://user:password@tools.invalid/mcp'})
            self.assertEqual(invalid.status_code, 400)
            removed = await client.put(f'/api/mcp/{server}', json={'clear_auth_token': True})
            self.assertEqual(removed.status_code, 200, removed.text)
            self.assertFalse((await client.get('/api/mcp')).json()[0]['has_auth_token'])
            self.assertEqual((await client.put('/api/mcp/999999', json={'name': 'Missing'})).status_code, 404)


if __name__ == '__main__':
    unittest.main()
