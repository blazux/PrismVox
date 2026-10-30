"""Reusable translations contain configuration, never caller conversations."""
import json
import os
from pathlib import Path
import unittest
from unittest.mock import AsyncMock, patch
import test_regressions as fixtures
import phrase_languages
import voice_config

class CatalogueTests(unittest.TestCase):
    def test_agent_and_console_ship_the_same_catalogue(self):
        source=Path(__file__).resolve().parents[2]/'agent/internal/locale/catalogue.json'
        self.assertEqual(json.loads(source.read_text()),phrase_languages.CATALOGUE)
        for catalogue in phrase_languages.CATALOGUE.values():
            self.assertTrue(phrase_languages.valid(phrase_languages.CATALOGUE['en'],catalogue))

@unittest.skipUnless(os.getenv('VOX_TEST_DATABASE_URL'),'requires disposable PostgreSQL')
class PhraseLanguageTests(unittest.IsolatedAsyncioTestCase):
    asyncSetUp=fixtures.DatabaseTests.asyncSetUp
    asyncTearDown=fixtures.DatabaseTests.asyncTearDown

    async def test_english_default_preserves_existing_configuration(self):
        with patch.dict(os.environ,{'ELEVENLABS_LANGUAGE':''}):
            settings=await voice_config.settings(self.pool)
            self.assertEqual(settings['voice_language'],'en')
            self.assertEqual(settings['voice_auto_language'],'true')
        await self.pool.execute("INSERT INTO config(key,value) VALUES('voice_language','fr'),('greeting','Bonjour personnalisé') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value")
        self.assertEqual((await voice_config.settings(self.pool))['voice_language'],'fr')
        self.assertEqual(await self.pool.fetchval("SELECT value FROM config WHERE key='greeting'"),'Bonjour personnalisé')

    async def test_standard_translations_are_ready_without_model(self):
        await phrase_languages.migrate(self.pool)
        complete=AsyncMock(side_effect=RuntimeError('offline'))
        result=await phrase_languages.prepare(self.pool,complete,['en','fr'])
        self.assertEqual(result['failed'],[])
        complete.assert_not_awaited()
        cached=await self.pool.fetchval('SELECT phrases FROM voice_phrase_translations WHERE language=$1','fr')
        self.assertEqual(json.loads(cached)['phrase_still_there'],'Êtes-vous toujours en ligne ?')

    async def test_custom_translation_is_persistent_and_invalidated_by_edits(self):
        await phrase_languages.migrate(self.pool)
        await self.pool.execute("INSERT INTO config(key,value) VALUES('greeting_known','Welcome to Acme, %s') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value")
        async def complete(pool,messages,max_tokens=0):
            self.assertEqual(len(messages),2)
            source=json.loads(messages[1]['content'])
            self.assertEqual(source['greeting_known'],'Welcome to Acme, %s')
            translated=dict(phrase_languages.CATALOGUE['fr']);translated['greeting_known']='Bienvenue chez Acme, %s'
            return {'content':json.dumps(translated)}
        model=AsyncMock(side_effect=complete)
        first=await phrase_languages.prepare(self.pool,model,['fr'])
        self.assertEqual(first['ready'],['fr']);self.assertEqual(model.await_count,1)
        second=await phrase_languages.prepare(self.pool,model,['fr'])
        self.assertEqual(second,first);self.assertEqual(model.await_count,1)
        await self.pool.execute("UPDATE config SET value='Hello %s, %s' WHERE key='greeting_known'")
        broken=AsyncMock(return_value={'content':json.dumps(phrase_languages.CATALOGUE['fr'])})
        changed=await phrase_languages.prepare(self.pool,broken,['fr'])
        self.assertNotEqual(changed['source_hash'],first['source_hash'])
        self.assertEqual(changed['failed'],['fr'])
        self.assertEqual(await self.pool.fetchval('SELECT count(*) FROM voice_phrase_translations'),1)
