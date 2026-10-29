"""Archive retrieval, privacy boundaries and persistent supervision (disposable DB)."""
import asyncio
import json
import os
import unittest
from unittest.mock import AsyncMock, patch
import httpx
import test_regressions as fixtures


@unittest.skipUnless(os.getenv('VOX_TEST_DATABASE_URL'), 'requires disposable PostgreSQL')
class HistoryArchiveTests(unittest.IsolatedAsyncioTestCase):
    asyncSetUp = fixtures.DatabaseTests.asyncSetUp
    asyncTearDown = fixtures.DatabaseTests.asyncTearDown

    async def seed(self):
        await self.pool.execute("""INSERT INTO call_logs(channel_id,caller_number,direction,started_at,ended_at,summary,turns,audience,prism_instance,owner_id)
        SELECT 'fixture-'||n,'+596596123456',CASE WHEN n%2=0 THEN 'outbound' ELSE 'inbound' END,
        '2026-10-20T12:00:00Z'::timestamptz+n*interval '1 minute','2026-10-20T12:01:00Z'::timestamptz+n*interval '1 minute',
        'old fixture call '||n,'[{"role":"caller","text":"fixture transcript"}]'::jsonb,'standard','prism-a',NULL FROM generate_series(1,37)n;
        INSERT INTO call_logs(channel_id,caller_number,summary,turns,audience,prism_instance,owner_id)
        VALUES ('private-a','111111111','owner seven','[{"role":"caller","text":"private mail"}]','personal','prism-a',7),
        ('private-b','222222222','owner eight','[]','personal','prism-a',8),
        ('private-other-deployment','333333333','other deployment','[]','personal','prism-b',7),
        ('legacy','444444444','legacy secret','[]','unclassified','',NULL),
        ('mono','555555555','personal owner zero','[]','personal','prism-a',0)
        """)

    async def test_archive_pagination_filters_and_privacy(self):
        await self.seed()
        archive=self.a.history_archive
        standard={'instance':'prism-a','standard':True}
        result=await archive.read(self.pool,{'limit':10,'number':'0596123456'},standard)
        self.assertEqual(result['total'],37)
        ids=[]
        while True:
            ids += [r['id'] for r in result['items']]
            if result['next_offset'] is None: break
            result=await archive.read(self.pool,{'limit':10,'number':'0596123456','offset':result['next_offset']},standard)
        self.assertEqual(len(set(ids)),37)
        result=await archive.read(self.pool,{'direction':'outbound','since':'2026-10-20T00:00:00Z','until':'2026-10-21T00:00:00Z'},standard)
        self.assertEqual(result['total'],18)
        details=await archive.read(self.pool,{'id':min(ids)},standard)
        self.assertIn('fixture transcript',str(details))
        private=await self.pool.fetchval("SELECT id FROM call_logs WHERE channel_id='private-a'")
        with self.assertRaises(self.a.HTTPException) as failure:
            await archive.read(self.pool,{'id':private},standard)
        self.assertEqual(failure.exception.status_code,404)
        owner={'instance':'prism-a','owner':7,'standard':False}
        result=await archive.read(self.pool,{},owner)
        self.assertEqual(result['total'],1)
        self.assertEqual(result['items'][0]['summary'],'owner seven')
        mono=await archive.read(self.pool,{}, {'instance':'prism-a','owner':0,'standard':False})
        self.assertEqual(mono['total'],1)
        self.assertEqual(mono['items'][0]['summary'],'personal owner zero')
        admin=await archive.read(self.pool,{}, {'admin':True})
        self.assertEqual(admin['total'],42)

    async def test_large_records_have_followable_text_pages(self):
        await self.seed()
        id=await self.pool.fetchval("SELECT min(id) FROM call_logs")
        content='A'*22000+'tail marker'
        await self.pool.execute('UPDATE call_logs SET turns=$2::jsonb WHERE id=$1',id,json.dumps([{'role':'caller','text':content}]))
        archive=self.a.history_archive;scope={'admin':True};chunks=[];cursor=0
        while True:
            page=await archive.read(self.pool,{'id':id,'limit':1,'text_offset':cursor},scope)
            item=page['items'][0];chunks.append(item['record_text'])
            self.assertLess(len(json.dumps(page)),13000)
            cursor=item['next_text_offset']
            if cursor is None: break
        self.assertEqual(json.loads(''.join(chunks))['text'],content)
        for query in [{'since':'yesterday'},{'number':'12'},{'direction':'sideways'},{'kind':'secret'}]:
            with self.assertRaises(self.a.HTTPException):
                await archive.read(self.pool,query,scope)

    async def test_long_conversation_messages_are_fully_retrievable(self):
        session='long-supervision'
        await self.pool.execute('INSERT INTO supervision_threads(session_id) VALUES($1)',session)
        content='début '+('x'*36000)+' fin importante'
        await self.pool.execute("INSERT INTO supervision_messages(session_id,role,content) VALUES($1,'user',$2)",session,content)
        scope={'admin':True}
        query={'kind':'conversation','query':'fin importante','limit':1}
        chunks=[]
        while True:
            result=await self.a.history_archive.read(self.pool,query,scope,session)
            self.assertEqual(result['total'],1)
            item=result['items'][0]
            chunks.append(item['record_text'])
            if item['next_text_offset'] is None:break
            query['text_offset']=item['next_text_offset']
        self.assertEqual(json.loads(''.join(chunks))['content'],content)
        with self.assertRaises(self.a.HTTPException) as failure:
            await self.a.history_archive.read(self.pool,{'kind':'conversation','text_offset':'bad'},scope,session)
        self.assertEqual(failure.exception.status_code,400)

    async def test_supervisor_reads_old_calls_and_survives_restart(self):
        await self.seed()
        calls=[]
        async def model(pool,messages,tools=None,max_tokens=1200):
            calls.append(messages.copy())
            if messages[-1]['role']!='tool':
                return {'content':None,'tool_calls':[{'id':'read-1','function':{'name':'history','arguments':'{"direction":"outbound","since":"2026-10-20T00:00:00Z","until":"2026-10-21T00:00:00Z"}'}}]}
            self.assertEqual(json.loads(messages[-1]['content'])['total'],18)
            return {'content':'18 outbound fixture calls.'}
        headers={'Authorization':'Basic Zml4dHVyZTpmaXh0dXJlLW9ubHk=', 'X-Prism-Scope':json.dumps({'instance':'prism-a','standard':True})}
        # Set the known UI username explicitly; no real deployment credentials.
        self.a.UI_USER=self.b.UI_USER='fixture'
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=self.a.app),base_url='http://fixture') as client:
            with patch.object(self.a,'_supervision_complete',side_effect=model):
                reply=await client.post('/api/chat',headers=headers,json={'session_id':'shared','message':'How many outbound calls that week?'})
            self.assertEqual(reply.status_code,200,reply.text)
            self.assertEqual(reply.json()['tools_used'][0]['name'],'history')
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=self.b.app),base_url='http://fixture') as client:
            history=await client.get('/api/supervision/conversation?session_id=shared',headers=headers)
            self.assertEqual(len(history.json()['items']),2)
            self.assertEqual(history.json()['items'][1]['tools'][0]['arguments']['direction'],'outbound')
            other=dict(headers);other['X-Prism-Scope']=json.dumps({'instance':'prism-b','standard':True})
            invisible=await client.get('/api/supervision/conversation?session_id=shared',headers=other)
            self.assertEqual(invisible.json()['items'],[])
        self.assertEqual(len(calls),2)
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=self.a.app),base_url='http://fixture') as client:
            legacy=await client.get('/api/call-logs?limit=25',headers=headers)
            self.assertEqual(len(legacy.json()['items']),25)
            self.assertIn('turns',legacy.json()['items'][0])
            self.assertIn('tools_used',legacy.json()['items'][0])

    async def test_context_compaction_preserves_archive_and_failure(self):
        scope={'admin':True};session=self.a.history_archive.session_key(scope,'long')
        await self.pool.execute('INSERT INTO supervision_threads(session_id) VALUES($1)',session)
        for n in range(12):
            await self.pool.execute("INSERT INTO supervision_messages(session_id,role,content) VALUES($1,$2,$3)",session,'user' if n%2==0 else 'assistant',f'fixture {n} '+('x'*3000))
        with patch.object(self.a,'_supervision_complete',new=AsyncMock(return_value={'content':'Retained fixture decisions'})):
            context=await self.a._supervision_context(self.pool,session,force=True)
        self.assertIn('Retained fixture decisions',str(context))
        self.assertEqual(await self.pool.fetchval('SELECT count(*) FROM supervision_messages WHERE session_id=$1',session),12)
        self.assertLess(sum(len(m['content']) for m in context),18001)
        await self.pool.execute("UPDATE supervision_threads SET summary='',before_id=0 WHERE session_id=$1",session)
        with patch.object(self.a,'_supervision_complete',new=AsyncMock(side_effect=RuntimeError('offline'))):
            with self.assertRaises(self.a.HTTPException):
                await self.a._supervision_context(self.pool,session,force=True)
        self.assertEqual(await self.pool.fetchval('SELECT before_id FROM supervision_threads WHERE session_id=$1',session),0)

    async def test_outbound_queue_is_owned_and_scoped(self):
        self.a.UI_USER='fixture'
        headers={'Authorization':'Basic Zml4dHVyZTpmaXh0dXJlLW9ubHk=', 'X-Prism-Scope':json.dumps({'instance':'prism-a','owner':7,'standard':False})}
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=self.a.app),base_url='http://fixture') as client:
            created=await client.post('/api/calls',headers=headers,json={'phone_number':'0596123456','mission':'fixture mission'})
            self.assertEqual(created.status_code,201,created.text)
            admin_headers=dict(headers);admin_headers['X-Prism-Scope']=json.dumps({'instance':'prism-a','admin':True})
            administrator=await client.get('/api/history',headers=admin_headers)
            self.assertEqual(administrator.status_code,200,administrator.text)
            legacy=await client.get('/api/call-logs',headers=headers)
            # No calls yet, then queue creation is still scoped independently.
            self.assertEqual(legacy.status_code,200)
            own=await client.get('/api/calls',headers=headers)
            self.assertEqual(own.json()['total'],1)
            other=dict(headers);other['X-Prism-Scope']=json.dumps({'instance':'prism-a','owner':8,'standard':False})
            invisible=await client.get('/api/calls',headers=other)
            self.assertEqual(invisible.json()['total'],0)
            forbidden=await client.delete('/api/calls/'+str(created.json()['id']),headers=other)
            self.assertEqual(forbidden.status_code,409)
            visible=await client.get('/api/calls',headers=headers)
            self.assertEqual(visible.json()['items'][0]['status'],'pending')

if __name__=='__main__':unittest.main()
