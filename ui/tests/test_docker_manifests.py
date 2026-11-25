"""Validate image promotion with a fake Docker CLI; never contact a registry."""
import json,os,subprocess,tempfile,unittest
from pathlib import Path
ROOT=Path(__file__).resolve().parents[2]
SCRIPT=ROOT/'scripts/publish-docker-manifest.sh'
IMAGE='fixture/prismvox-asterisk'
FAKE='''#!/usr/bin/env python3
import json,os,sys
from pathlib import Path
args=sys.argv[1:]
with open(os.environ['MOCK_CALLS'],'a') as log:log.write(json.dumps(args)+'\\n')
if args[:3] not in [['buildx','imagetools','create'],['buildx','imagetools','inspect']]:raise SystemExit(3)
if '--dry-run' in args or args[2]=='inspect':
 platforms=[('linux','amd64'),('linux','arm64')]
 mode=os.environ.get('MOCK_MODE','valid')
 if mode=='missing-arm':platforms=platforms[:1]
 if mode=='extra-arch':platforms.append(('linux','riscv64'))
 if mode=='attestations':platforms.extend([('unknown','unknown'),('unknown','unknown')])
 if mode=='bad-published' and args[2]=='inspect':platforms=platforms[:1]
 print(json.dumps({'schemaVersion':2,'manifests':[{'platform':{'os':o,'architecture':a}}for o,a in platforms]}))
'''
class PublishTests(unittest.TestCase):
 def execute(self,tags=None,mode='valid',missing=False,duplicate=False,malformed=False):
  with tempfile.TemporaryDirectory(prefix='prismvox-manifest-test-')as directory:
   p=Path(directory);(p/'bin').mkdir();(p/'bin/docker').write_text(FAKE);(p/'bin/docker').chmod(0o755)
   for arch,digit in [('amd64','a'),('arm64','b')]:
    (p/'digests'/arch).mkdir(parents=True)
    if missing and arch=='arm64':continue
    (p/'digests'/arch/(digit*64)).touch()
   if duplicate:(p/'digests/arm64'/('c'*64)).touch()
   if malformed:(p/'digests/amd64'/('a'*64)).rename(p/'digests/amd64/not-a-digest')
   env=dict(os.environ,PATH=str(p/'bin')+':'+os.environ['PATH'],RUNNER_TEMP=str(p),DIGESTS_DIR=str(p/'digests'),IMAGE=IMAGE,TAGS=IMAGE+':unstable'if tags is None else tags,MOCK_MODE=mode,MOCK_CALLS=str(p/'calls.jsonl'))
   result=subprocess.run(['bash',str(SCRIPT)],env=env,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
   calls=[json.loads(line)for line in (p/'calls.jsonl').read_text().splitlines()]if(p/'calls.jsonl').exists()else[]
   publishes=[c for c in calls if c[2]=='create'and'--dry-run'not in c]
   return result,publishes,calls
 def test_main_stable_prerelease_tags_preserved(self):
  for suffixes in [('unstable',),('v0.1.3','latest'),('v0.2.0-rc.1',)]:
   with self.subTest(suffixes=suffixes):
    tags='\n'.join(IMAGE+':'+v for v in suffixes);r,p,c=self.execute(tags=tags);self.assertEqual(r.returncode,0,r.stderr);self.assertEqual(len(p),1)
    self.assertEqual([p[0][i+1]for i,v in enumerate(p[0])if v=='--tag'],tags.splitlines())
    self.assertEqual(p[0][-2:],[IMAGE+'@sha256:'+'a'*64,IMAGE+'@sha256:'+'b'*64])
    self.assertEqual(sum(c[2]=='inspect'for c in c),len(suffixes))
 def test_missing_duplicate_or_invalid_digest_never_promotes(self):
  for changes in [{'missing':True},{'duplicate':True},{'malformed':True}]:
   with self.subTest(changes=changes):
    r,p,c=self.execute(**changes);self.assertNotEqual(r.returncode,0);self.assertEqual(p,[]);self.assertEqual(c,[])
 def test_unexpected_or_empty_tags_never_promote(self):
  for tags in ['other/repository:latest','']:
   r,p,c=self.execute(tags=tags);self.assertNotEqual(r.returncode,0);self.assertEqual(p,[]);self.assertEqual(c,[])
 def test_wrong_platforms_are_rejected_before_promotion(self):
  for mode in ['missing-arm','extra-arch']:
   r,p,c=self.execute(mode=mode);self.assertNotEqual(r.returncode,0);self.assertEqual(p,[])
 def test_attestations_do_not_count_as_runnable_platforms(self):
  r,p,c=self.execute(mode='attestations');self.assertEqual(r.returncode,0,r.stderr);self.assertEqual(len(p),1)
 def test_registry_result_is_checked_after_promotion(self):
  r,p,c=self.execute(mode='bad-published');self.assertNotEqual(r.returncode,0);self.assertEqual(len(p),1)
if __name__=='__main__':unittest.main()
