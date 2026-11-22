"""Queue totals/paging and redial state use only disposable call metadata."""
import json
import os
import unittest

import httpx
import test_regressions as fixtures


@unittest.skipUnless(os.getenv("VOX_TEST_DATABASE_URL"), "requires a disposable PostgreSQL database")
class OutboundPageTests(unittest.IsolatedAsyncioTestCase):
    asyncSetUp = fixtures.DatabaseTests.asyncSetUp
    asyncTearDown = fixtures.DatabaseTests.asyncTearDown

    def client(self):
        return httpx.AsyncClient(transport=httpx.ASGITransport(app=self.a.app),
                                 base_url="http://fixture.invalid", auth=("admin", "fixture-only"))

    def headers(self, **scope):
        return {"X-Prism-Scope": json.dumps(dict(instance="fixture-a", **scope))}

    async def test_global_pending_count_and_stable_pages_are_scoped(self):
        await self.pool.execute("""INSERT INTO call_tasks(phone_number,mission,status,audience,prism_instance,owner_id,scheduled_at,updated_at)
            SELECT '1001', 'Own fixture ' || n, CASE WHEN n<=150 THEN 'pending' ELSE 'done' END,
                   'personal','fixture-a',7,'2030-01-01','2030-01-01' FROM generate_series(1,205) AS n""")
        await self.pool.execute("""INSERT INTO call_tasks(phone_number,mission,status,audience,prism_instance,owner_id)
            SELECT '1002','Other fixture','pending','personal','fixture-a',8 FROM generate_series(1,100)""")
        await self.pool.execute("""INSERT INTO call_tasks(phone_number,mission,status,audience,prism_instance,owner_id)
            SELECT '1003','Other instance','pending','personal','fixture-b',7 FROM generate_series(1,40)""")
        await self.pool.execute("""INSERT INTO call_tasks(phone_number,mission,status,audience,prism_instance)
            SELECT '1004','Standard fixture','pending','standard','fixture-a' FROM generate_series(1,30)""")
        seen = []
        async with self.client() as client:
            for offset, count in [(0,100), (100,100), (200,5)]:
                response = await client.get(f"/api/calls?limit=100&offset={offset}", headers=self.headers(owner=7))
                self.assertEqual(response.status_code, 200, response.text)
                data = response.json()
                self.assertEqual((data["total"], data["pending_total"], data["offset"]), (205,150,offset))
                self.assertEqual(len(data["items"]), count)
                seen.extend(item["id"] for item in data["items"])
                repeated = await client.get(f"/api/calls?limit=100&offset={offset}", headers=self.headers(owner=7))
                self.assertEqual(repeated.json(), data)
            self.assertEqual(len(set(seen)), 205)
            first_ids = [item["id"] for item in (await client.get("/api/calls", headers=self.headers(owner=7))).json()["items"]]
            self.assertEqual(first_ids, sorted(first_ids, reverse=True))
            standard = (await client.get("/api/calls", headers=self.headers(standard=True))).json()
            self.assertEqual((standard["total"], standard["pending_total"]), (30,30))
            zero = (await client.get("/api/calls", headers=self.headers(owner=0))).json()
            self.assertEqual((zero["total"], zero["pending_total"], zero["items"]), (0,0,[]))
            outside = (await client.get("/api/calls?offset=9999", headers=self.headers(owner=7))).json()
            self.assertEqual((outside["total"], outside["pending_total"], outside["items"]), (205,150,[]))

    async def test_retry_clears_current_result_preserves_history_and_creation_receipt(self):
        async with self.client() as client:
            for state in ["done", "no_answer", "failed", "cancelled"]:
                body = dict(phone_number="1001", mission="Fixture retry", request_id="retry-fixture-" + state)
                headers = self.headers(owner=7)
                created = await client.post("/api/calls", json=body, headers=headers)
                self.assertEqual(created.status_code, 201, created.text)
                task_id = created.json()["id"]
                log_id = await self.pool.fetchval("""INSERT INTO call_logs(channel_id,direction,summary,turns,audience,prism_instance,owner_id)
                    VALUES($1,'outbound','Previous fixture outcome','[{"role":"assistant","content":"Fixture transcript"}]',
                           'personal','fixture-a',7) RETURNING id""", "fixture-" + state)
                await self.pool.execute("UPDATE call_tasks SET status=$1,attempts=3,result_summary='Previous fixture outcome',call_log_id=$2 WHERE id=$3",state,log_id,task_id)
                before = dict(await self.pool.fetchrow("SELECT * FROM call_tasks WHERE id=$1", task_id))
                history = dict(await self.pool.fetchrow("SELECT * FROM call_logs WHERE id=$1", log_id))
                forbidden = await client.post(f"/api/calls/{task_id}/retry", headers=self.headers(owner=8))
                self.assertEqual(forbidden.status_code, 409)
                self.assertEqual(dict(await self.pool.fetchrow("SELECT * FROM call_tasks WHERE id=$1", task_id)), before)
                response = await client.post(f"/api/calls/{task_id}/retry", headers=headers)
                self.assertEqual(response.status_code, 200, response.text)
                retried = dict(await self.pool.fetchrow("SELECT * FROM call_tasks WHERE id=$1", task_id))
                self.assertEqual((retried["status"], retried["attempts"], retried["result_summary"], retried["call_log_id"]), ("pending",0,None,None))
                for field in ["mission", "phone_number", "creation_request_id", "creation_payload_hash", "created_at"]:
                    self.assertEqual(retried[field], before[field])
                self.assertEqual(dict(await self.pool.fetchrow("SELECT * FROM call_logs WHERE id=$1", log_id)), history)
                # Recovery of the creation receipt must not undo this redial state.
                replayed = await client.post("/api/calls", json=body, headers=headers)
                self.assertEqual(replayed.json(), created.json())
                self.assertEqual(dict(await self.pool.fetchrow("SELECT * FROM call_tasks WHERE id=$1", task_id)), retried)
                busy = await client.post(f"/api/calls/{task_id}/retry", headers=headers)
                self.assertEqual(busy.status_code, 409)


if __name__ == "__main__":
    unittest.main()
