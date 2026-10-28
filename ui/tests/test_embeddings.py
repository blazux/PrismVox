"""External provider protocol, legacy migration and model-safe re-indexing."""
import hashlib
import json
import os
import unittest
from unittest.mock import patch

from aiohttp import web
import httpx
import test_regressions as fixtures


@unittest.skipUnless(os.getenv('VOX_TEST_DATABASE_URL'), 'requires disposable PostgreSQL')
class EmbeddingTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        await fixtures.DatabaseTests.asyncSetUp(self)
        self.a.secret_store.load_key(create=True)
        self.dimension = 3
        self.failure = False
        self.malformed = False
        self.embedding_requests = []
        self.change_settings = False

        async def provider(request):
            payload = await request.json()
            self.embedding_requests.append((request.path,request.headers.get('Authorization'),payload))
            if self.failure:
                return web.json_response({'error':'fixture-embedding-secret'},status=401)
            if self.malformed:
                return web.json_response({'data':[{'index':0,'embedding':[0,0,0]}]})
            if self.change_settings and payload['input'] != ['PrismVox']:
                await self.pool.execute("UPDATE config SET value='concurrent-model' WHERE key='embedding_model'")
            data=[]
            for i,text in enumerate(payload['input']):
                vec=[0.0]*self.dimension
                vec[1 if 'second' in text else 0]=1.0
                data.append({'index':i,'embedding':vec})
            return web.json_response({'data':list(reversed(data))})

        app=web.Application()
        app.router.add_post('/v1/embeddings',provider)
        app.router.add_post('/other/v1/embeddings',provider)
        self.embedding_runner=web.AppRunner(app)
        await self.embedding_runner.setup()
        site=web.TCPSite(self.embedding_runner,'127.0.0.1',0)
        await site.start()
        self.endpoint='http://127.0.0.1:'+str(site._server.sockets[0].getsockname()[1])+'/v1'
        self.client=httpx.AsyncClient(transport=httpx.ASGITransport(app=self.a.app),base_url='http://fixture',auth=('admin','fixture-only'))

    async def asyncTearDown(self):
        await self.client.aclose()
        await self.embedding_runner.cleanup()
        await fixtures.DatabaseTests.asyncTearDown(self)

    async def save(self,model='fixture-model',**extra):
        values={'embedding_url':self.endpoint,'embedding_model':model,**extra}
        response=await self.client.put('/api/embeddings',json={'values':values})
        self.assertEqual(response.status_code,200,response.text)
        return response.json()

    async def seed(self,count=35):
        col=await self.pool.fetchval("INSERT INTO rag_collections(name) VALUES('fixture knowledge') RETURNING id")
        await self.pool.execute("INSERT INTO rag_chunks(collection_id,content,chunk_index) SELECT $1,'fixture document '||n,n FROM generate_series(1,$2::int)n",col,count)
        return col

    async def test_provider_credentials_validation_and_auth(self):
        await self.save(embedding_api_key='fixture-embedding-secret')
        request=self.embedding_requests[-1]
        self.assertEqual(request[0],'/v1/embeddings')
        self.assertEqual(request[1],'Bearer fixture-embedding-secret')
        self.assertEqual(request[2],{'model':'fixture-model','input':['PrismVox'],'encoding_format':'float'})
        vectors=await self.a.embedding_config.embed(await self.a.embedding_config.settings(self.pool),['first','second'])
        self.assertEqual(vectors,[[1,0,0],[0,1,0]])
        stored=await self.pool.fetchval("SELECT value FROM config WHERE key='embedding_api_key'")
        self.assertTrue(stored.startswith('enc:v1:'))
        for path in ('/api/embeddings','/api/config'):
            response=await self.client.get(path)
            self.assertNotIn(stored,response.text)
            self.assertNotIn('fixture-embedding-secret',response.text)
        response=await self.client.get('/api/embeddings')
        self.assertTrue(response.json()['api_key_configured'])
        await self.save() # blank password preserves the credential
        self.assertEqual(self.embedding_requests[-1][1],'Bearer fixture-embedding-secret')
        await self.client.post('/api/embeddings/test',json={'values':{'embedding_url':self.endpoint.replace('/v1','/other/v1')}})
        self.assertIsNone(self.embedding_requests[-1][1]) # URL changes do not forward the saved key
        self.assertEqual((await self.client.get('/api/embeddings')).json()['embedding_url'],self.endpoint)
        for path in ('/api/embeddings','/api/config'):
            response=await self.client.get(path,auth=None,headers={'Authorization':'Basic invalid'})
            self.assertEqual(response.status_code,401)
        self.failure=True
        response=await self.client.put('/api/embeddings',json={'values':{'embedding_model':'broken-model'}})
        self.assertEqual(response.status_code,502)
        self.assertNotIn('fixture-embedding-secret',response.text)
        self.assertEqual((await self.client.get('/api/embeddings')).json()['embedding_model'],'fixture-model')
        self.failure=False;self.malformed=True
        response=await self.client.post('/api/embeddings/test',json={'values':{}})
        self.assertEqual(response.status_code,502)

    async def test_migrate_legacy_vectors_and_reindex_all_in_bounded_pages(self):
        col=await self.seed()
        # Reproduce the schema from before provider configuration existed.
        await self.pool.execute('DROP INDEX rag_chunks_embedding_key_idx; ALTER TABLE rag_chunks DROP COLUMN embedding_key; ALTER TABLE rag_chunks ALTER COLUMN embedding TYPE vector(384); CREATE INDEX ON rag_chunks USING ivfflat (embedding vector_cosine_ops) WITH (lists=1)')
        legacy=json.dumps([1]+[0]*383)
        await self.pool.execute('UPDATE rag_chunks SET embedding=$1::vector',legacy)
        await self.a.embedding_config.migrate(self.pool)
        await self.a.embedding_config.migrate(self.pool)
        self.assertEqual(await self.pool.fetchval('SELECT count(*) FROM rag_chunks'),35)
        self.assertEqual(await self.pool.fetchval('SELECT vector_dims(embedding) FROM rag_chunks LIMIT 1'),384)
        before=await self.save()
        self.assertEqual(before['pending'],35)
        after=0;seen=[]
        while True:
            response=await self.client.post('/api/rag/embed-all',json={'after_id':after,'limit':10})
            self.assertEqual(response.status_code,200,response.text)
            result=response.json();seen.append(result['embedded'])
            after=result['next_after']
            if after is None: break
        self.assertEqual(seen,[10,10,10,5])
        self.assertEqual(result['indexed'],35)
        self.assertEqual(result['pending'],0)
        self.assertEqual(await self.pool.fetchval('SELECT min(vector_dims(embedding)) FROM rag_chunks'),3)
        # Another model and another dimension invalidate old vectors without losing text.
        self.dimension=2
        result=await self.save('second-model')
        self.assertEqual(result['pending'],35)
        self.assertEqual(await self.pool.fetchval('SELECT count(*) FROM rag_chunks WHERE content LIKE $1','fixture document%'),35)
        response=await self.client.post('/api/rag/embed-all',json={'limit':64})
        self.assertEqual(response.json()['pending'],0)
        cfg=await self.a.embedding_config.settings(self.pool)
        # Unrelated dimensions must not be fed into the cosine operator.
        await self.pool.execute("INSERT INTO rag_chunks(collection_id,content,embedding,embedding_key) VALUES($1,'foreign index','[1,0,0]'::vector,'foreign')",col)
        result=await self.a._kb_search(self.pool,'fixture document')
        self.assertIn('fixture document',result)
        self.assertNotIn('foreign index',result)
        key=self.a.embedding_config.index_key({'embedding_url':'http://provider:11434/v1','embedding_model':'fixture-model','embedding_dimensions':3})
        self.assertEqual(key,hashlib.sha256(b'http://provider:11434/v1\nfixture-model\n3').hexdigest())
        self.assertEqual(cfg['embedding_dimensions'],2)

    async def test_upload_order_and_settings_changed_during_indexing(self):
        await self.save()
        col=await self.seed(1)
        response=await self.client.post(f'/api/rag/collections/{col}/upload',files={'file':('note.txt',b'first document text','text/plain')})
        self.assertEqual(response.status_code,200,response.text)
        self.assertEqual(response.json()['embedded'],1)
        row=await self.pool.fetchrow("SELECT embedding::text AS vec,embedding_key FROM rag_chunks WHERE source='note.txt'")
        self.assertEqual(row['vec'],'[1,0,0]')
        self.assertTrue(row['embedding_key'])
        self.change_settings=True
        response=await self.client.post('/api/rag/embed-all')
        self.assertEqual(response.status_code,409,response.text)
        self.assertEqual(await self.pool.fetchval('SELECT count(*) FROM rag_chunks WHERE embedding IS NULL'),1)
        # Configuration cannot be bypassed through the generic config writer.
        await self.client.put('/api/config',json={'values':{'embedding_url':'http://bad.invalid/v1','embedding_api_key':'bad'}})
        self.assertEqual((await self.client.get('/api/embeddings')).json()['embedding_url'],self.endpoint)

    async def test_env_bootstrap_and_no_provider_fallback_preserve_document_text(self):
        await self.seed(1)
        result=await self.a._kb_search(self.pool,'fixture')
        self.assertIn('fixture document',result)
        with patch.dict(os.environ,{'EMBEDDING_BASE_URL':self.endpoint,'EMBEDDING_MODEL':'environment-model','EMBEDDING_API_KEY':''}):
            cfg=await self.a.embedding_config.prepared(self.pool)
            self.assertEqual(cfg['embedding_dimensions'],3)
        # The resolved bootstrap settings now persist independently of env/restarts.
        cfg=await self.a.embedding_config.settings(self.pool)
        self.assertEqual(cfg['embedding_model'],'environment-model')
        self.assertEqual(cfg['embedding_url'],self.endpoint)
