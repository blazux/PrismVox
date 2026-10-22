// npm install --prefix /tmp/vox-dom-tests jsdom
// NODE_PATH=/tmp/vox-dom-tests/node_modules node ui/tests/dom-regressions.cjs
const fs = require('fs');
const path = require('path');
const assert = require('assert/strict');
const {JSDOM} = require('jsdom');
const src = fs.readFileSync(path.join(__dirname, '../app/static/index.html'), 'utf8');
const build = src.slice(src.indexOf('function buildConvRows(row)'), src.indexOf('function toggleConv(rid)'));
const load = src.slice(src.indexOf('async function loadMCPTools(serverId)'), src.indexOf('async function probeMCP(serverId)'));
const esc = src.split('\n').find(s => s.startsWith('function esc(s)'));
const events = src.slice(src.indexOf("document.addEventListener('click', event =>"), src.indexOf('// ── Init ─'));
const dom = new JSDOM('<table><tbody id="rows"></tbody></table><div id="mcp-tools-7"></div>', {runScripts:'dangerously', url:'http://fixture.invalid'});
const w = dom.window;
w.eval(esc + '\nfunction channelBadge(){return "api";}\n' + build + '\n' + load + '\n' + events);
const payloads = ["');window.__injected=true;//", '\"><img src=x onerror="window.__injected=true">', 'normal/session', 'a-b', 'a/b'];
w.deleteConversation = id => { w.deleted = id; };
const ids = new Set();
for (const session_id of payloads) {
  w.document.querySelector('#rows').innerHTML = w.buildConvRows({session_id, started_at:'2026-10-13T00:00:00Z', channel:'api', contact:'fixture', turns:[{role:'user', text:'fixture'}]});
  const button = w.document.querySelector('[data-delete-conversation]');
  assert.equal(button.getAttribute('onclick'), null);
  assert.equal(button.dataset.deleteConversation, session_id);
  button.click();
  assert.equal(w.deleted, session_id);
  assert.equal(w.__injected, undefined);
  assert.equal(w.document.querySelector('img'), null);
  const id = w.document.querySelector('tr').id;
  assert.ok(!ids.has(id));
  ids.add(id);
}
(async () => {
  for (const name of payloads) {
    w.api = async () => [{name, description:name, enabled:true}];
    w.toggleMCPTool = (server, tool, enabled) => { w.toggled = [server, tool, enabled]; };
    await w.loadMCPTools(7);
    const input = w.document.querySelector('[data-mcp-tool]');
    assert.equal(input.getAttribute('onchange'), null);
    input.checked = false;
    input.dispatchEvent(new w.Event('change', {bubbles:true}));
    assert.deepEqual(w.toggled, [7, name, false]);
    assert.equal(w.__injected, undefined);
    assert.equal(w.document.querySelector('img'), null);
  }
  w.close();
  console.log('DOM regressions passed: identifiers remain data; delete and MCP toggles work.');
})().catch(e => { w.close(); console.error(e); process.exitCode = 1; });
