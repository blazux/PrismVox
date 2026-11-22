"""Creation receipts are scoped, atomic and independent of mutable task state.

Uses the disposable PostgreSQL fixture; no calls or model requests are made.
"""
import asyncio
import json
import os
import unittest

import httpx
import test_regressions as fixtures


@unittest.skipUnless(os.getenv("VOX_TEST_DATABASE_URL"), "requires a disposable PostgreSQL database")
class OutboundRequestTests(unittest.IsolatedAsyncioTestCase):
    asyncSetUp = fixtures.DatabaseTests.asyncSetUp
    asyncTearDown = fixtures.DatabaseTests.asyncTearDown

    def client(self, app=None):
        return httpx.AsyncClient(transport=httpx.ASGITransport(app=(app or self.a).app),
                                 base_url="http://fixture.invalid", auth=("admin", "fixture-only"))

    def payload(self):
        return dict(phone_number="0596123456", mission="Confirm fixture appointment",
                    contact_name="Fixture person", max_attempts=3, request_id="fixture-create-1")

    async def test_concurrent_replay_and_lost_acknowledgement_return_one_receipt(self):
        async with self.client() as first, self.client(self.b) as second:
            results = await asyncio.gather(first.post("/api/calls", json=self.payload()),
                                           second.post("/api/calls", json=self.payload()))
            self.assertEqual([r.status_code for r in results], [201, 201])
            receipt = results[0].json()
            self.assertEqual(results[1].json(), receipt)
            # The caller did not retain either acknowledgement, then asks again.
            recovered = await second.post("/api/calls", json=self.payload())
            self.assertEqual(recovered.json(), receipt)
            self.assertEqual(await self.pool.fetchval("SELECT count(*) FROM call_tasks"), 1)

    async def test_same_id_different_creation_intent_conflicts_without_changes(self):
        async with self.client() as client:
            original = await client.post("/api/calls", json=self.payload())
            task_id = original.json()["id"]
            before = dict(await self.pool.fetchrow("SELECT * FROM call_tasks WHERE id=$1", task_id))
            for field, value in [("phone_number", "0596987654"), ("mission", "Different mission"),
                                 ("contact_name", "Someone else"), ("max_attempts", 5),
                                 ("scheduled_at", "2030-01-01T12:00:00Z")]:
                changed = self.payload()
                changed[field] = value
                response = await client.post("/api/calls", json=changed)
                self.assertEqual(response.status_code, 409, response.text)
            self.assertEqual(dict(await self.pool.fetchrow("SELECT * FROM call_tasks WHERE id=$1", task_id)), before)
            self.assertEqual(await self.pool.fetchval("SELECT count(*) FROM call_tasks"), 1)

    async def test_receipt_never_reopens_or_overwrites_a_processed_task(self):
        async with self.client() as client:
            original = await client.post("/api/calls", json=self.payload())
            task_id = original.json()["id"]
            await self.pool.execute("""UPDATE call_tasks SET status='done', attempts=2,
                result_summary='Fixture completed', mission='Worker annotation',
                scheduled_at='2031-01-01T12:00:00Z', updated_at='2031-01-01T13:00:00Z'
                WHERE id=$1""", task_id)
            before = dict(await self.pool.fetchrow("SELECT * FROM call_tasks WHERE id=$1", task_id))
            response = await client.post("/api/calls", json=self.payload())
            self.assertEqual(response.status_code, 201)
            self.assertEqual(response.json(), original.json())
            self.assertEqual(dict(await self.pool.fetchrow("SELECT * FROM call_tasks WHERE id=$1", task_id)), before)

    async def test_creation_keys_are_scoped_by_instance_audience_and_owner(self):
        scopes = [None, {"instance": "fixture-a", "admin": True},
                  {"instance": "fixture-b", "admin": True},
                  {"instance": "fixture-a", "owner": 7},
                  {"instance": "fixture-a", "owner": 8},
                  {"instance": "fixture-a", "owner": 0}]
        ids = []
        async with self.client() as client:
            for scope in scopes:
                headers = {"X-Prism-Scope": json.dumps(scope)} if scope else {}
                first = await client.post("/api/calls", headers=headers, json=self.payload())
                self.assertEqual(first.status_code, 201, first.text)
                repeated = await client.post("/api/calls", headers=headers, json=self.payload())
                self.assertEqual(repeated.json(), first.json())
                ids.append(first.json()["id"])
            # A non-admin standard scope sees the same standard receipt in its instance.
            shared = await client.post("/api/calls", headers={"X-Prism-Scope": json.dumps(
                {"instance": "fixture-a", "standard": True})}, json=self.payload())
            self.assertEqual(shared.json()["id"], ids[1])
            self.assertEqual(len(set(ids)), len(scopes))
            self.assertEqual(await self.pool.fetchval("SELECT count(*) FROM call_tasks"), len(scopes))

    async def test_legacy_clients_and_validation_keep_existing_contract(self):
        async with self.client() as client:
            body = self.payload()
            del body["request_id"]
            first = await client.post("/api/calls", json=body)
            second = await client.post("/api/calls", json=body)
            self.assertEqual(first.status_code, 201)
            self.assertEqual(second.status_code, 201)
            self.assertEqual(set(first.json()), {"id"})
            self.assertNotEqual(first.json(), second.json())
            self.assertEqual(await self.pool.fetchval("SELECT count(*) FROM call_tasks WHERE creation_request_id IS NULL AND creation_payload_hash IS NULL"), 2)
            for key in ["", "a" * 129, "with space", "é", "-leading"]:
                invalid = self.payload()
                invalid["request_id"] = key
                response = await client.post("/api/calls", json=invalid)
                self.assertEqual(response.status_code, 400)
            self.assertEqual(await self.pool.fetchval("SELECT count(*) FROM call_tasks"), 2)

    async def test_normalized_intent_and_schedule_are_stable(self):
        async with self.client() as client:
            body = self.payload()
            body["scheduled_at"] = "2030-01-01T12:00:00+00:00"
            first = await client.post("/api/calls", json=body)
            self.assertEqual(first.status_code, 201, first.text)
            body.update(phone_number=" 0596123456 ", mission=" Confirm fixture appointment ",
                        contact_name=" Fixture person ", scheduled_at="2030-01-01T08:00:00-04:00")
            second = await client.post("/api/calls", json=body)
            self.assertEqual(second.status_code, 201, second.text)
            self.assertEqual(second.json(), first.json())

    async def test_additive_migration_preserves_old_rows_and_can_repeat(self):
        await self.pool.execute("ALTER TABLE call_tasks DROP COLUMN creation_request_id, DROP COLUMN creation_payload_hash")
        task_id = await self.pool.fetchval("INSERT INTO call_tasks(phone_number,mission,status) VALUES('1001','Old fixture','done') RETURNING id")
        before = dict(await self.pool.fetchrow("SELECT * FROM call_tasks WHERE id=$1", task_id))
        await self.a._ensure_schema(self.pool)
        await self.a._ensure_schema(self.pool)
        after = dict(await self.pool.fetchrow("SELECT * FROM call_tasks WHERE id=$1", task_id))
        self.assertIsNone(after.pop("creation_request_id"))
        self.assertIsNone(after.pop("creation_payload_hash"))
        self.assertEqual(after, before)
        async with self.client() as client:
            first = await client.post("/api/calls", json=self.payload())
            second = await client.post("/api/calls", json=self.payload())
            self.assertEqual(first.status_code, 201, first.text)
            self.assertEqual(second.json(), first.json())


if __name__ == "__main__":
    unittest.main()
