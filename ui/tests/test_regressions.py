"""Run with unittest; DB cases require VOX_TEST_DATABASE_URL (disposable DB).

Fixtures use a separate, randomly named schema and a fake local model: no calls,
provider charges, existing data or production services are involved.
"""
import asyncio
import base64
import concurrent.futures
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch
import unittest
import uuid

from aiohttp import web
import asyncpg
import httpx

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "ui/app"))
import secret_store


def load_app():
    spec = importlib.util.spec_from_file_location("vox_test_" + uuid.uuid4().hex, ROOT / "ui/app/main.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class SecretTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.old = os.environ.get("SECRET_KEY_PATH")
        os.environ["SECRET_KEY_PATH"] = self.tmp.name + "/secret.key"

    def tearDown(self):
        if self.old is None:
            os.environ.pop("SECRET_KEY_PATH", None)
        else:
            os.environ["SECRET_KEY_PATH"] = self.old
        self.tmp.cleanup()

    def test_concurrent_key_creation_and_authenticated_encryption(self):
        with concurrent.futures.ThreadPoolExecutor(8) as executor:
            keys = list(executor.map(lambda _: secret_store.load_key(create=True), range(32)))
        self.assertTrue(all(k == keys[0] for k in keys))
        self.assertEqual(secret_store.key_path().stat().st_mode & 0o777, 0o600)
        encrypted = secret_store.encrypt("fixture credential", "mcp:42")
        self.assertNotIn("fixture credential", encrypted)
        self.assertEqual(secret_store.decrypt(encrypted, "mcp:42"), "fixture credential")
        for field in ("mcp:43", "sip_password"):
            with self.assertRaises(RuntimeError):
                secret_store.decrypt(encrypted, field)
        with self.assertRaises(RuntimeError):
            secret_store.decrypt(encrypted[:-5] + "AAAAA", "mcp:42")
        secret_store.key_path().write_bytes(bytes(32))
        with self.assertRaises(RuntimeError):
            secret_store.decrypt(encrypted, "mcp:42")
        secret_store.key_path().unlink()
        with self.assertRaises(RuntimeError):
            secret_store.decrypt(encrypted, "mcp:42")
        self.assertFalse(secret_store.key_path().exists())

    def test_cross_language_vector(self):
        key = secret_store.key_path()
        key.write_bytes(bytes(range(32)))
        key.chmod(0o600)
        vector = "enc:v1:AAECAwQFBgcICQoLNHu4b62AtnLuYfT51I0dA_e_5lhOcUzonIgzhLDb4NL5MUVv"
        self.assertEqual(secret_store.decrypt(vector, "mcp:42"), "synthetic credential")
        key.chmod(0o644)
        with self.assertRaises(ValueError):
            secret_store.load_key()


class PaginationTests(unittest.IsolatedAsyncioTestCase):
    async def test_catalogue_pages_and_bad_cursor(self):
        app = load_app()
        class Session:
            async def list_tools(self, cursor=None):
                return SimpleNamespace(tools=[SimpleNamespace(name=cursor or "first")], nextCursor="second" if cursor is None else None)
        self.assertEqual([t.name for t in await app._list_mcp_tools(Session())], ["first", "second"])
        class Broken:
            async def list_tools(self, cursor=None):
                return SimpleNamespace(tools=[], nextCursor="loop")
        with self.assertRaises(ValueError):
            await app._list_mcp_tools(Broken())


class ConsoleTests(unittest.IsolatedAsyncioTestCase):
    async def test_standalone_and_connected_entry_pages(self):
        app = load_app()
        app.PRISM_URL = ""
        standalone = await app.index()
        self.assertIn('/static/app.css', standalone)
        self.assertNotIn('cdn.jsdelivr.net', standalone)
        app.PRISM_URL = "http://prism:8080"
        app.PRISM_PUBLIC_URL = "https://prism.example.test"
        connected = await app.index()
        self.assertIn('href="https://prism.example.test"', connected)
        self.assertIn("Your phone settings live in Prism", connected)
        self.assertNotIn('href="http://prism:8080"', connected)
        app.PRISM_PUBLIC_URL = ""
        self.assertNotIn('href="http://prism:8080"', await app.index())
        app.PRISM_PUBLIC_URL = "javascript:alert(1)"
        self.assertNotIn('href="javascript:', await app.index())


class VoiceBackendTests(unittest.IsolatedAsyncioTestCase):
    async def test_active_engine_metadata_and_failure_reporting(self):
        app = load_app()
        app.UI_PASSWORD = "fixture-only"
        info = {"status": "ok"}
        async def health(request):
            return web.json_response(info)
        server = web.Application()
        server.router.add_get("/health", health)
        runner = web.AppRunner(server)
        await runner.setup()
        site = web.TCPSite(runner, "127.0.0.1", 0)
        await site.start()
        app.TTS_URL = "http://127.0.0.1:" + str(site._server.sockets[0].getsockname()[1])
        app._pool = SimpleNamespace(fetch=AsyncMock(return_value=[]))
        self.enterContext(patch.dict(os.environ, {'TTS_URL':app.TTS_URL,'TTS_PROVIDER':'service'}))
        try:
            async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app.app), base_url="http://fixture.invalid") as client:
                self.assertEqual((await client.get("/api/tts/backend")).status_code, 401)
                for metadata, backend, clone in [
                    ({"status":"ok", "engine":"elevenlabs"}, "elevenlabs", True),
                    ({"status":"ok", "engine":"elevenlabs", "clone_enabled":False}, "elevenlabs", False),
                    ({"status":"ok", "engine":"melo", "clone_enabled":False}, "melo", False),
                    ({"status":"ok"}, "local", False),
                ]:
                    info.clear(); info.update(metadata)
                    response = await client.get("/api/tts/backend", auth=("admin", "fixture-only"))
                    self.assertEqual(response.status_code, 200)
                    self.assertEqual(response.json(), {"backend":backend,"clone_enabled":clone})
                info.clear(); info.update(status="no_api_key", engine="elevenlabs")
                self.assertEqual((await client.get("/api/tts/backend", auth=("admin","fixture-only"))).status_code, 503)
                app.TTS_URL = "http://127.0.0.1:1"
                os.environ["TTS_URL"] = app.TTS_URL
                response = await client.get("/api/tts/backend", auth=("admin","fixture-only"))
                self.assertEqual(response.status_code, 502)
                self.assertIn("unavailable", response.json()["detail"])
        finally:
            await runner.cleanup()


@unittest.skipUnless(shutil.which("envsubst"), "requires envsubst")
class TLSBootstrapTests(unittest.TestCase):
    def test_new_legacy_explicit_persisted_and_invalid_choices(self):
        for legacy, explicit, expected in ((False, "", "yes"), (True, "", "no"), (True, "yes", "yes")):
            with self.subTest(legacy=legacy, explicit=explicit), tempfile.TemporaryDirectory() as tmp:
                tpl, target = Path(tmp, "tpl"), Path(tmp, "etc")
                shutil.copytree(ROOT / "asterisk/etc", tpl)
                target.mkdir()
                dyn = target / "dynamic"
                dyn.mkdir()
                if legacy:
                    (dyn / "pjsip_trunk.conf").write_text("existing trunk\n")
                script = (ROOT / "asterisk/docker-entrypoint.sh").read_text()
                script = script.replace("/etc/asterisk-tpl", str(tpl)).replace("/etc/asterisk", str(target))
                script = script.replace("/usr/share/asterisk/sounds/custom", tmp + "/sounds")
                entry = Path(tmp, "entry.sh")
                entry.write_text(script)
                env = {"PATH": os.environ["PATH"], "SIP_REGISTRAR_IP": "192.0.2.1", "SIP_TLS_VERIFY_SERVER": explicit}
                result = subprocess.run(["bash", str(entry), "true"], env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("verify_server = " + expected, (target / "pjsip.conf").read_text())
                self.assertEqual((dyn / "tls_verify_server").read_text().strip(), expected)
                if legacy:
                    self.assertEqual((dyn / "pjsip_trunk.conf").read_text(), "existing trunk\n")
                env["SIP_TLS_VERIFY_SERVER"] = ""
                result = subprocess.run(["bash", str(entry), "true"], env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("verify_server = " + expected, (target / "pjsip.conf").read_text())
                env["SIP_TLS_VERIFY_SERVER"] = "invalid"
                self.assertNotEqual(subprocess.run(["bash", str(entry), "true"], env=env, capture_output=True).returncode, 0)
                env.update(SIP_TLS_VERIFY_SERVER="yes", SIP_TLS_CA_FILE=tmp + "/missing-ca")
                self.assertNotEqual(subprocess.run(["bash", str(entry), "true"], env=env, capture_output=True).returncode, 0)


@unittest.skipUnless(os.getenv("VOX_TEST_DATABASE_URL"), "requires a disposable PostgreSQL database")
class DatabaseTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.old_key = os.environ.get("SECRET_KEY_PATH")
        os.environ["SECRET_KEY_PATH"] = self.tmp.name + "/secret.key"
        dsn = os.environ["VOX_TEST_DATABASE_URL"]
        self.schema = "review_" + uuid.uuid4().hex
        self.admin = await asyncpg.connect(dsn)
        await self.admin.execute('CREATE SCHEMA "' + self.schema + '"')
        self.pool = await asyncpg.create_pool(dsn, min_size=1, max_size=5,
            server_settings={"search_path": self.schema + ",public"})
        await self.pool.execute((ROOT / "db/init.sql").read_text())
        self.a, self.b = load_app(), load_app()  # independent HTTP/HTTPS process globals
        for app in (self.a, self.b):
            app._pool = self.pool
            app.UI_PASSWORD = "fixture-only"
            app.AGENT_API_KEY = "fixture-agent-only"
        await self.a._ensure_schema(self.pool)
        await self.a._ensure_schema(self.pool)
        await self.pool.execute("UPDATE config SET value='false' WHERE key IN ('rag_enabled','mcp_enabled')")
        self.requests = []
        async def fake_llm(request):
            payload = await request.json()
            self.requests.append(payload)
            messages = payload["messages"]
            if any(m.get("content") == "find fixture" for m in messages) and messages[-1]["role"] != "tool":
                msg = {"role": "assistant", "content": None, "tool_calls": [{"id": "fixture-search", "type": "function", "function": {"name": "search_knowledge_base", "arguments": '{"query":"fixture"}'}}]}
            else:
                await asyncio.sleep(.01)
                msg = {"role": "assistant", "content": "fixture reply"}
            return web.json_response({"choices": [{"message": msg}]})
        server = web.Application()
        server.router.add_post("/v1/chat/completions", fake_llm)
        async def fake_mcp(request):
            if request.headers.get("Authorization") != "Bearer fixture-mcp-token":
                return web.Response(status=401)
            if request.method == "DELETE":
                return web.Response(status=204)
            if request.method != "POST":
                return web.Response(status=405)
            rpc = await request.json()
            method = rpc["method"]
            if method == "notifications/initialized":
                return web.Response(status=202)
            if method == "initialize":
                result = {"protocolVersion": "2025-03-26", "capabilities": {"tools": {}}, "serverInfo": {"name": "fixture", "version": "1"}}
            else:
                self.assertEqual(request.headers.get("Mcp-Session-Id"), "fixture-session")
                if method == "tools/list":
                    name = "second" if rpc.get("params", {}).get("cursor") else "first"
                    result = {"tools": [{"name": name, "description": "fixture tool", "inputSchema": {"type": "object"}}]}
                    if name == "first":
                        result["nextCursor"] = "page-two"
                elif method == "tools/call":
                    result = {"content": [{"type": "text", "text": "fixture tool result"}], "isError": False}
                else:
                    return web.Response(status=400)
            return web.json_response({"jsonrpc": "2.0", "id": rpc["id"], "result": result}, headers={"Mcp-Session-Id": "fixture-session"})
        server.router.add_route("*", "/mcp", fake_mcp)
        self.runner = web.AppRunner(server)
        await self.runner.setup()
        self.site = web.TCPSite(self.runner, "127.0.0.1", 0)
        await self.site.start()
        port = self.site._server.sockets[0].getsockname()[1]
        self.a.LLM_BASE_URL = self.b.LLM_BASE_URL = f"http://127.0.0.1:{port}/v1"
        self.mcp_url = f"http://127.0.0.1:{port}/mcp"

    async def asyncTearDown(self):
        await self.runner.cleanup()
        await self.pool.close()
        await self.admin.execute('DROP SCHEMA "' + self.schema + '" CASCADE')
        await self.admin.close()
        if self.old_key is None:
            os.environ.pop("SECRET_KEY_PATH", None)
        else:
            os.environ["SECRET_KEY_PATH"] = self.old_key
        self.tmp.cleanup()

    async def test_legacy_secret_migration_and_write_only_apis(self):
        await self.pool.execute("INSERT INTO config(key,value) VALUES('sip_password','fixture-sip')")
        sid = await self.pool.fetchval("INSERT INTO mcp_servers(name,type,url,auth_token) VALUES('fixture','http','http://fixture.invalid','fixture-token') RETURNING id")
        await self.pool.execute("INSERT INTO mcp_tools(server_id,name,enabled) VALUES($1,'fixture-tool',false)", sid)
        await secret_store.migrate(self.pool)
        sip = await self.pool.fetchval("SELECT value FROM config WHERE key='sip_password'")
        token = await self.pool.fetchval("SELECT auth_token FROM mcp_servers WHERE id=$1", sid)
        self.assertTrue(sip.startswith(secret_store.PREFIX))
        self.assertEqual(secret_store.decrypt(sip, "sip_password"), "fixture-sip")
        self.assertEqual(secret_store.decrypt(token, f"mcp:{sid}"), "fixture-token")
        await secret_store.migrate(self.pool)
        self.assertEqual(await self.pool.fetchval("SELECT value FROM config WHERE key='sip_password'"), sip)
        self.assertFalse(await self.pool.fetchval("SELECT enabled FROM mcp_tools WHERE server_id=$1", sid))
        self.assertEqual((await self.a.list_mcp())[0]["auth_token"], "")
        self.assertTrue((await self.a.list_mcp())[0]["has_auth_token"])
        self.assertEqual((await self.a.get_config())["sip_password"], "")
        new = await self.a.create_mcp(self.a.MCPCreate(name="new", type="http", auth_token="new-fixture-token"))
        stored = await self.pool.fetchval("SELECT auth_token FROM mcp_servers WHERE id=$1", new["id"])
        self.assertEqual(secret_store.decrypt(stored, f"mcp:{new['id']}"), "new-fixture-token")
        cfg = dict(sip_registrar="fixture.invalid", sip_registrar_ip="192.0.2.1", sip_username="fixture", sip_password="", sip_domain="fixture.invalid")
        await self.a.update_sip(self.a.SipConfig(**cfg))
        stored = await self.pool.fetchval("SELECT value FROM config WHERE key='sip_password'")
        self.assertEqual(secret_store.decrypt(stored, "sip_password"), "fixture-sip")
        self.a.ASTERISK_DYNAMIC_DIR = self.tmp.name
        Path(self.tmp.name, "pjsip_trunk.conf.tpl").write_text("password=${SIP_PASSWORD}\n")
        self.a._render_trunk({"sip_password": stored})
        rendered = Path(self.tmp.name, "pjsip_trunk.conf")
        self.assertEqual(rendered.read_text(), "password=fixture-sip\n")
        self.assertEqual(rendered.stat().st_mode & 0o777, 0o600)
        secret_store.key_path().unlink()
        with self.assertRaises(RuntimeError):
            await secret_store.migrate(self.pool)
        self.assertFalse(secret_store.key_path().exists())

    async def test_reset_cross_process_concurrency_and_delete(self):
        async def turn(app, text, session="fixture-session"):
            return await app.agent_chat(app.AgentChatMessage(message=text, session_id=session))
        await turn(self.a, "old context")
        await self.b.reset_agent_chat("fixture-session")
        await turn(self.a, "new context")
        self.assertEqual([m["content"] for m in self.requests[-1]["messages"][1:]], ["new context"])
        turns = json.loads(await self.pool.fetchval("SELECT turns FROM chat_sessions WHERE session_id='fixture-session'"))
        self.assertEqual(len(turns), 4)
        self.assertEqual(turns[0]["text"], "old context")
        # More simultaneous requests than pool slots: no deadlock or lost writes.
        await asyncio.wait_for(asyncio.gather(*(turn(self.a if i % 2 else self.b, f"parallel-{i}") for i in range(8))), 10)
        turns = json.loads(await self.pool.fetchval("SELECT turns FROM chat_sessions WHERE session_id='fixture-session'"))
        self.assertEqual(len(turns), 20)
        self.assertEqual(sum(t["text"].startswith("parallel-") for t in turns), 8)
        await self.b.delete_conversation("fixture-session")
        await turn(self.a, "after deletion")
        self.assertEqual([m["content"] for m in self.requests[-1]["messages"][1:]], ["after deletion"])

    async def test_existing_history_migration(self):
        await self.pool.execute("ALTER TABLE chat_sessions DROP COLUMN context_start")
        transcript = [{"role": "user", "text": "existing fixture"}]
        await self.pool.execute("INSERT INTO chat_sessions(session_id,turns) VALUES('legacy',$1::jsonb)", json.dumps(transcript))
        await self.a._ensure_schema(self.pool)
        await self.a._ensure_schema(self.pool)
        row = await self.pool.fetchrow("SELECT turns,context_start FROM chat_sessions WHERE session_id='legacy'")
        self.assertEqual(json.loads(row["turns"]), transcript)
        self.assertEqual(row["context_start"], 0)

    async def test_sip_partial_client_preserves_encrypted_password_and_hidden_fields(self):
        await secret_store.migrate(self.pool)
        initial = dict(self.a.SIP_ENV_DEFAULTS, sip_registrar="sbc.example.test", sip_registrar_ip="192.0.2.1",
            sip_registrar_match="192.0.2.0/24", sip_username="fixture", sip_domain="example.test",
            sip_transport_mode="udp", sip_udp_port="5090", sip_tls_port="5072", sip_transfer_method="refer")
        initial["sip_password"] = secret_store.encrypt("fixture-sip", "sip_password")
        for key, value in initial.items():
            await self.pool.execute("INSERT INTO config(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=$2", key, value)
        body = dict(sip_registrar="sbc.example.test", sip_registrar_ip="192.0.2.1", sip_username="fixture", sip_domain="example.test")
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=self.a.app), base_url="http://fixture.invalid", auth=("admin","fixture-only")) as client:
            for password in (None, "", "__unchanged__"):
                payload = dict(body)
                if password is not None: payload["sip_password"] = password
                response = await client.put("/api/sip", json=payload)
                self.assertEqual(response.status_code, 200, response.text)
                cfg = dict(await self.pool.fetch("SELECT key,value FROM config WHERE key=ANY($1::text[])", self.a.SIP_KEYS))
                self.assertEqual(secret_store.decrypt(cfg["sip_password"], "sip_password"), "fixture-sip")
                for key in ("sip_transport_mode","sip_udp_port","sip_registrar_match","sip_tls_port","sip_transfer_method"):
                    self.assertEqual(cfg[key], initial[key])
            response = await client.put("/api/sip", json=dict(body, sip_transport_mode="tls", sip_udp_port="5060", sip_registrar_match=""))
            self.assertEqual(response.status_code, 200, response.text)
            cfg = await self.a.get_sip()
            self.assertEqual((cfg["sip_transport_mode"],cfg["sip_udp_port"],cfg["sip_registrar_match"]), ("tls","5060","192.0.2.1"))
            self.assertEqual(cfg["sip_password"], "")
            await self.pool.execute("DELETE FROM config WHERE key='sip_password'")
            response = await client.put("/api/sip", json=body)
            self.assertEqual(response.status_code, 400)

    async def test_python_mcp_initialization_pagination_and_probe(self):
        await secret_store.migrate(self.pool)
        sid = (await self.a.create_mcp(self.a.MCPCreate(name="fixture", type="http", url=self.mcp_url, auth_token="fixture-mcp-token")))["id"]
        await self.pool.execute("INSERT INTO mcp_tools(server_id,name,enabled) VALUES($1,'first',false),($1,'removed',true)", sid)
        result = await self.a.probe_mcp(sid)
        self.assertEqual([t["name"] for t in result["tools"]], ["first", "second"])
        rows = await self.pool.fetch("SELECT name,enabled FROM mcp_tools WHERE server_id=$1 ORDER BY name", sid)
        self.assertEqual([(r["name"], r["enabled"]) for r in rows], [("first", False), ("second", True)])
        specs, routing = await self.a._agent_gather_mcp_tools(self.pool)
        self.assertEqual([t["function"]["name"] for t in specs], ["second"])
        self.assertEqual(await self.a._mcp_call_tool(routing["second"], "second", {}), "fixture tool result")

    async def test_shim_rag_stream_and_auth(self):
        searched = []
        async def search(pool, query):
            searched.append(query)
            return "fixture knowledge"
        self.a._kb_search = search
        await self.pool.execute("UPDATE config SET value='true' WHERE key='rag_enabled'")
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=self.a.app), base_url="http://fixture.invalid") as client:
            self.assertEqual((await client.get("/api/config")).status_code, 401)
            self.assertEqual((await client.post("/v1/chat/completions", json={})).status_code, 401)
            basic = base64.b64encode("admin:wrong-é".encode()).decode()
            self.assertEqual((await client.get("/api/config", headers={"Authorization": "Basic " + basic})).status_code, 401)
            for stream in (False, True):
                r = await client.post("/v1/chat/completions", headers={"Authorization": "Bearer fixture-agent-only"},
                    json={"messages": [{"role": "user", "content": "find fixture"}], "stream": stream})
                self.assertEqual(r.status_code, 200, r.text)
                self.assertIn("fixture reply", r.text)
                self.assertEqual(self.requests[-1]["messages"][-1]["content"], "fixture knowledge")
                self.assertIn("search_knowledge_base", [t["function"]["name"] for t in self.requests[-1]["tools"]])
        self.assertEqual(searched, ["fixture", "fixture"])


if __name__ == "__main__":
    unittest.main()
