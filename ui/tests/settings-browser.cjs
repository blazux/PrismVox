// UI-only fixtures: no live configuration, provider or phone calls.
const {chromium}=require('playwright');
const fs=require('fs'),path=require('path'),assert=require('node:assert/strict');
const root=path.resolve(__dirname,'../app/static');
(async()=>{
 const browser=await chromium.launch({headless:true,...(process.env.CHROMIUM_EXECUTABLE_PATH?{executablePath:process.env.CHROMIUM_EXECUTABLE_PATH}:{})});
 try {
 const page=await browser.newPage({viewport:{width:1280,height:900}}),errors=[],writes=[];
 page.on('pageerror',e=>errors.push(e.message));
 const config={system_prompt:'Inbound fixture',outbound_system_prompt:'Outbound fixture',llm_model_standard:'fast',llm_model_oncall:'capable',greeting:'Hello',rag_enabled:'true',mcp_enabled:'false',transfer_dialog_turns:'3'};
 let fail=false;
 await page.route('**/*',route=>{
  const req=route.request(),url=new URL(req.url());
  assert.equal(url.hostname,'fixture.invalid');
  if(url.pathname.startsWith('/static/'))return route.fulfill({path:path.join(root,url.pathname.slice(8))});
  if(url.pathname==='/')return route.fulfill({path:path.join(root,'index.html')});
  if(url.pathname==='/api/voice/editor')return route.fulfill({contentType:'text/html',body:'<p>Speech providers</p>'});
  if(req.method()==='PUT'){
   writes.push(req.postDataJSON());
   if(fail)return route.fulfill({status:500,contentType:'application/json',body:'{"detail":"Fixture failure"}'});
   Object.assign(config,req.postDataJSON().values);
  }
  const fixtures={'/api/config':config,'/api/dictionary':{},'/api/contacts':[], '/api/tts/voices':{voices:['Demo'],current:'Demo'},'/api/tts/backend':{backend:'melo',clone_enabled:false},'/api/health':{db:'ok',tts:'ok',stt:'ok',llm:'ok'},'/api/sip':{},'/api/sip/status':{},'/api/runtime-status':{sip_ok:true,active_calls:0,uptime_secs:7200,calls_today:4,calls_since_start:4,total_calls:12,avg_duration_secs:80,transfer_pct:25,calls_per_day:[{day:'Mon',count:3},{day:'Tue',count:5},{day:'Wed',count:4}],end_reasons:[{reason:'hung_up',count:9},{reason:'transferred',count:3}],top_callers:[]}};
  return route.fulfill({contentType:'application/json',body:JSON.stringify(fixtures[url.pathname]||{})});
 });
 await page.goto('http://fixture.invalid/');
 await page.waitForFunction(()=>document.getElementById('cfg-system_prompt').value==='Inbound fixture');
 if(process.env.VOX_DOC_SCREENSHOTS){
  const output=process.env.VOX_DOC_SCREENSHOTS;fs.mkdirSync(output,{recursive:true});
  await page.locator('#rt-total').filter({hasText:'12'}).waitFor();
  await page.screenshot({path:path.join(output,'overview.png'),fullPage:true,animations:'disabled'});
  await page.locator('nav button[onclick*="setup"]').click();
  await page.locator('#cfg-system_prompt').fill('You are the receptionist for Example Workshop. Answer briefly and use the knowledge base for company information.');
  await page.locator('#cfg-outbound_system_prompt').fill('Carry out the call mission. Ask one question at a time and confirm the outcome before ending the call.');
  await page.locator('#cfg-llm_model_standard').fill('example-vox');
  await page.screenshot({path:path.join(output,'agent.png'),fullPage:true});
  await page.evaluate(()=>loadConfig());
 }
 await page.locator('nav button[onclick*="setup"]').click();
 assert.equal(await page.locator('#cfg-llm_model_oncall').count(),0);
 assert.equal(await page.locator('#cfg-llm_url_oncall').count(),0);
 await page.locator('#cfg-system_prompt').fill('Unsaved agent draft');
 await page.locator('#tab-setup .subtabs button[onclick*="voicecfg"]').click();
 await page.locator('#cfg-greeting').fill('Welcome');
 await page.locator('#voice-form button[type=submit]').click();
 await page.locator('#voice-form [data-config-status]').filter({hasText:'Saved'}).waitFor();
 assert.equal(writes.length,1);assert.equal(writes[0].values.greeting,'Welcome');assert.ok(!('system_prompt' in writes[0].values));
 await page.locator('#cfg-greeting').fill('Discard this');
 await page.locator('#voice-form').getByText('Discard changes').click();
 await page.waitForFunction(()=>document.getElementById('cfg-greeting').value==='Welcome');
 assert.equal(await page.locator('#cfg-system_prompt').inputValue(),'Unsaved agent draft');
 await page.locator('#tab-setup .subtabs button[onclick*="agentcfg"]').click();
 assert.equal(await page.locator('#cfg-system_prompt').inputValue(),'Unsaved agent draft');
 await page.locator('#agent-form button[type=submit]').click();
 await page.locator('#agent-form [data-config-status]').filter({hasText:'Saved'}).waitFor();
 assert.ok(!('greeting' in writes[1].values));assert.ok(!('transfer_dialog_turns' in writes[1].values));
 await page.locator('#tab-setup .subtabs button[onclick*="transfers"]').click();
 fail=true;await page.locator('#transfer-form button[type=submit]').click();
 await page.locator('#transfer-form [data-config-status]').filter({hasText:'Could not save'}).waitFor();
 assert.deepEqual(Object.keys(writes[2].values),['transfer_dialog_turns']);
 for(const width of [1280,390]){
  await page.setViewportSize({width,height:900});
  for(const name of ['agentcfg','voicecfg','transfers','sip']){
   await page.locator(`#tab-setup .subtabs button[onclick*="${name}"]`).click();
   assert.ok(await page.locator('#subtab-'+name).isVisible());
   assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),name+' overflow '+width);
  }
 }
 await page.locator('#tab-setup .subtabs button[onclick*="agentcfg"]').click();
 await page.screenshot({path:'/tmp/prismvox-settings-mobile.png',fullPage:true});
 await page.setViewportSize({width:1280,height:900});
 await page.screenshot({path:'/tmp/prismvox-settings-desktop.png',fullPage:true});
 assert.deepEqual(errors,[]);console.log('Settings: scoped save, discard, errors, navigation and mobile checks passed');
 } finally {await browser.close();}
})().catch(e=>{console.error(e);process.exit(1)});
