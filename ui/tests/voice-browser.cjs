// Requires Playwright; all API responses are synthetic.
const {chromium}=require('playwright');
const fs=require('fs'),assert=require('assert/strict');
const path=require('path');
const root=path.resolve(__dirname,'../app/static')+'/';
const output=process.env.VOX_UI_SCREENSHOTS||'/tmp';
fs.mkdirSync(output,{recursive:true});
(async()=>{
 const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_EXECUTABLE_PATH?{executablePath:process.env.CHROMIUM_EXECUTABLE_PATH}:{})});
 const page=await browser.newPage({viewport:{width:1000,height:800}});
 const errors=[],calls=[];let fail=false;
 page.on('pageerror',e=>errors.push(e.message));
 let state={voice_stt_provider:'service',voice_tts_provider:'service',voice_stt_url:'http://stt:9000',voice_tts_url:'http://tts:8000/synthesize',voice_language:'fr',voice_stt_model:'scribe_v2_realtime',voice_tts_model:'eleven_flash_v2_5',voice_id:'',api_key_configured:false};
 await page.route('**/*',async route=>{
  const req=route.request(),path=new URL(req.url()).pathname;
  assert.equal(new URL(req.url()).hostname,'fixture.invalid');
  if(path.endsWith('/voice/editor'))return route.fulfill({contentType:'text/html',body:fs.readFileSync(root+'voice.html','utf8')});
  if(path.endsWith('/voice/style'))return route.fulfill({contentType:'text/css',body:fs.readFileSync(root+'app.css','utf8')});
  if(path.endsWith('/voice/connect')){
   calls.push({path,method:req.method(),body:req.postDataJSON()});
   return route.fulfill({status:fail?502:200,contentType:'application/json',body:JSON.stringify(fail?{detail:'Provider rejected the API key'}:{models:[{id:'model-test',name:'Test speech'}],voices:[{id:'voice-test',name:'Test voice'}]})});
  }
  if(path.endsWith('/voice')){
   if(req.method()==='PUT'){calls.push({path,method:req.method(),body:req.postDataJSON()});Object.assign(state,req.postDataJSON().values);state.api_key_configured=!!state.voice_api_key||state.api_key_configured;delete state.voice_api_key;}
   return route.fulfill({contentType:'application/json',body:JSON.stringify(state)});
  }
  return route.fulfill({status:404,body:''});
 });
 await page.goto('http://fixture.invalid/api/voice/editor');
 await page.locator('#save:not(:disabled)').waitFor();
 assert.equal(await page.locator('#eleven-settings').isVisible(),false);
 await page.locator('#voice_stt_provider').selectOption('elevenlabs');await page.locator('#voice_tts_provider').selectOption('elevenlabs');
 await page.locator('#voice_api_key').fill('synthetic-key');
 await page.locator('#connect').click();await page.getByText('Connected. Choose a model and voice, then save.').waitFor();
 assert.equal(calls.filter(c=>c.method==='PUT').length,0,'connect persisted settings');
 await page.locator('#voice_tts_model').fill('model-test');await page.locator('#voice_id').selectOption('voice-test');
 await page.locator('#voice_stability').fill('0.65');
 await page.locator('#save').click();await page.getByText('Saved · applies to new calls').waitFor();
 assert.equal(await page.locator('#voice_api_key').inputValue(),'');
 assert.match(await page.locator('#voice_api_key').getAttribute('placeholder'),/Key saved/);
 await page.reload();await page.locator('#save:not(:disabled)').waitFor();
 assert.equal(await page.locator('#voice_id').inputValue(),'voice-test');
 assert.equal(await page.locator('#voice_tts_model').inputValue(),'model-test');
 assert.equal(await page.locator('#voice_stability').inputValue(),'0.65');
 await page.screenshot({path:path.join(output,'prismvox-voice-desktop.png'),fullPage:true});
 fail=true;await page.locator('#connect').click();await page.getByText('Provider rejected the API key').waitFor();assert.equal(await page.locator('#connect').isEnabled(),true);
 await page.setViewportSize({width:390,height:844});await page.evaluate(()=>document.documentElement.dataset.theme='light');
 await page.screenshot({path:path.join(output,'prismvox-voice-mobile.png'),fullPage:true});
 assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'mobile overflow');
 fail=false;await page.goto('http://fixture.invalid/api/vox/voice/editor');await page.locator('#save:not(:disabled)').waitFor();
 await page.locator('#connect').click();await page.getByText('Connected. Choose a model and voice, then save.').waitFor();
 assert(calls.some(c=>c.path==='/api/vox/voice/connect'),'docked proxy prefix not used');
 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({passed:true,checks:['connect without saving','save and reload','masked credential','provider errors release buttons','mobile layout','docked API prefix'],screenshots:[path.join(output,'prismvox-voice-desktop.png'),path.join(output,'prismvox-voice-mobile.png')]}));
 await browser.close();
})().catch(e=>{console.error(e);process.exit(1)});
