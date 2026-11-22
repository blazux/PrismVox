"""A mission receipt stays readable by its owner after it leaves recent pages."""
import json
import os
import unittest

import httpx
import test_regressions as fixtures


@unittest.skipUnless(os.getenv('VOX_TEST_DATABASE_URL'), 'requires disposable PostgreSQL')
class CallTaskDetailTests(unittest.IsolatedAsyncioTestCase):
    asyncSetUp = fixtures.DatabaseTests.asyncSetUp
    asyncTearDown = fixtures.DatabaseTests.asyncTearDown

    async def test_finished_task_is_read_by_id_with_owner_and_instance_checks(self):
        task = await self.pool.fetchval('''INSERT INTO call_tasks(phone_number,mission,status,
            audience,prism_instance,owner_id,result_summary,attempts,updated_at)
            VALUES('1001','Fixture mission','done','personal','fixture-a',7,'Confirmed fixture result',2,'2020-01-01')
            RETURNING id''')
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=self.a.app),
                base_url='http://fixture.invalid', auth=('admin', 'fixture-only')) as client:
            headers = {'X-Prism-Scope': json.dumps({'instance': 'fixture-a', 'owner': 7})}
            result = await client.get(f'/api/calls/{task}', headers=headers)
            self.assertEqual(result.status_code, 200, result.text)
            self.assertEqual(result.json()['result_summary'], 'Confirmed fixture result')
            self.assertEqual(result.json()['attempts'], 2)
            self.assertIn('call_log_id', result.json())
            for scope in ({'instance': 'fixture-a', 'owner': 8}, {'instance': 'fixture-b', 'owner': 7},
                          {'instance': 'fixture-a', 'standard': True}):
                hidden = await client.get(f'/api/calls/{task}', headers={'X-Prism-Scope': json.dumps(scope)})
                self.assertEqual(hidden.status_code, 404)
                self.assertNotIn('Confirmed fixture result', hidden.text)


if __name__ == '__main__':
    unittest.main()
