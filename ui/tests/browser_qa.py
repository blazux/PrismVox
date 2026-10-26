"""Optional Playwright smoke check: serve ui/app locally on port 4891 first.
All API responses are synthetic; external requests are blocked.
"""
import json
import os
from pathlib import Path
from playwright.sync_api import sync_playwright
out=Path(os.getenv('VOX_UI_SCREENSHOTS', '/tmp/prismvox-ui-preview'));out.mkdir(exist_ok=True)
config={'system_prompt':'You are the company receptionist. Answer using approved knowledge.','greeting':'Hello, how can I help?','llm_model_standard':'example-fast','llm_model_oncall':'example-capable','rag_enabled':'true','mcp_enabled':'true','admin_chat_prompt':'Help the operator review calls.','transfer_dialog_turns':'3'}
for k,v in {'phrase_hold':'Please hold.','phrase_transfer_now':'Connecting you now.','phrase_connecting':'%s is taking your call.','phrase_unknown_contact':'I cannot reach %s.','phrase_farewell':'Goodbye!','phrase_ask_name':'May I have your name?','phrase_ask_reason':'What is your call about?','phrase_still_there':'Are you still there?','phrase_announce':'%s is calling about %s.'}.items():config[k]=v
fixtures={
'/api/config':config,
'/api/health':dict.fromkeys(['db','stt','tts','llm'],'ok'),
'/api/runtime-status':{'sip_ok':True,'active_calls':0,'uptime_secs':84731,'calls_today':14,'calls_since_start':14,'total_calls':53,'avg_duration_secs':96,'transfer_pct':42,'calls_per_day':[{'day':d,'count':n} for d,n in zip(['Mon','Tue','Wed','Thu','Fri','Sat','Sun'],[4,9,5,12,6,3,14])],'end_reasons':[{'reason':'hung_up','count':30},{'reason':'transferred','count':23}],'top_callers':[{'number':'Demo caller','count':7}]},
'/api/tts/voices':{'voices':['Demo.wav'],'current':'Demo.wav'},
'/api/sip':{'sip_registrar':'sbc.example.test','sip_registrar_ip':'192.0.2.1','sip_domain':'example.test','sip_username':'switchboard','sip_password':'','sip_transport_mode':'tls','sip_tls_port':'5061','sip_udp_port':'5060','sip_transfer_method':'bridge'},
'/api/sip/status':{'endpoint_state':'online'},
'/api/rag/collections':[{'id':1,'name':'Public information','description':'Opening hours and company services','enabled':True,'chunk_count':12}],
'/api/mcp':[{'id':1,'name':'Service tools','type':'http','url':'http://tools.example.test/mcp','enabled':True}],
'/api/mcp/1/tools':[{'name':'lookup_appointment','description':'Find an appointment','enabled':True}],
'/api/contacts':[{'id':1,'name':'Alex Morgan','phone_number':'1001','notes':'Customer support','enabled':True,'transfer_type':'attended'}],
'/api/calls':{'items':[],'total':0},'/api/call-logs':{'items':[],'total':0},'/api/conversations':{'items':[],'total':0},
}
with sync_playwright() as p:
 options={'headless':True}
 if os.getenv('CHROMIUM_EXECUTABLE_PATH'):options['executable_path']=os.environ['CHROMIUM_EXECUTABLE_PATH']
 browser=p.chromium.launch(**options)
 page=browser.new_page(viewport={'width':1440,'height':1000})
 errors=[];external=[]
 page.on('pageerror',lambda e:errors.append(str(e)))
 def route(r):
  from urllib.parse import urlsplit
  url=urlsplit(r.request.url)
  if '/api/' in url.path:r.fulfill(status=200,content_type='application/json',body=json.dumps(fixtures.get(url.path,{})))
  elif url.hostname not in ['127.0.0.1','localhost']:external.append(r.request.url);r.abort()
  else:r.continue_()
 page.route('**/*',route)
 page.add_init_script("localStorage.setItem('theme','dark')")
 page.goto(os.getenv('VOX_UI_PREVIEW_URL', 'http://127.0.0.1:4891/static/index.html'))
 page.locator('#rt-total').filter(has_text='53').wait_for()
 page.screenshot(path=str(out/'overview-dark.png'),full_page=True)
 for name in ['setup','rag','mcp','outbound','logs','conversations','chat']:
  page.locator(f'nav button[onclick*="\'{name}\'"]').click()
  page.wait_for_timeout(80)
  assert page.locator('#tab-'+name).is_visible(),name
  assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'),name+' horizontal overflow'
 page.locator('nav button[onclick*="\'setup\'"]').click()
 page.locator('#tab-setup button[onclick*="\'agentcfg\'"]').click()
 page.screenshot(path=str(out/'agent-dark.png'),full_page=True)
 assert page.locator('#cfg-rag_enabled').evaluate("e => getComputedStyle(e).appearance")=='none'
 page.locator('#theme-btn').click()
 page.screenshot(path=str(out/'agent-light.png'),full_page=True)
 page.locator('#tab-setup button[onclick*="\'sip\'"]').click()
 page.screenshot(path=str(out/'sip-light.png'),full_page=True)
 page.set_viewport_size({'width':390,'height':844})
 page.screenshot(path=str(out/'sip-mobile.png'),full_page=True)
 assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'),'mobile SIP overflow'
 page.locator('#tab-setup button[onclick*="\'transfers\'"]').click()
 page.locator('#new-contact-details').evaluate('(e)=>e.open=true')
 assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'),'mobile directory overflow'
 assert not errors,errors
 assert not external,external
 print(json.dumps({'pages_checked':9,'themes':['dark','light'],'mobile_width':390,'javascript_errors':errors,'external_requests':external,'screenshots':str(out)}))
 browser.close()
