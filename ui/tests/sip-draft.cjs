const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../app/static/index.html'), 'utf8');
const script = source.slice(source.indexOf('const SIP_FIELDS'), source.indexOf('async function loadSipStatus'));

function fixture() {
  const elements = {'sip-status': {textContent: '', style: {}},
                    'sip-tls-port-label': {style: {}}, 'sip-udp-port-label': {style: {}}};
  const requests = [];
  const context = vm.createContext({
    document: {getElementById: id => elements[id]},
    get: id => elements[id].value,
    setText: (id, value) => {elements[id].value = value;},
    hide: id => {elements[id].style.display = 'none';},
    api(method, url, body) {
      return new Promise((resolve, reject) => requests.push({method, url, body, resolve, reject}));
    },
  });
  vm.runInContext(script, context);
  vm.runInContext('SIP_FIELDS', context).forEach(k => {elements['sip-' + k] = {value: ''};});
  function edit(key, value) {
    elements['sip-' + key].value = value;
    context.markSipEdit({target: {id: 'sip-' + key}});
  }
  return {context, elements, requests, edit};
}
const saved = {sip_registrar: 'saved.example.test', sip_username: 'saved-user',
               sip_transport_mode: 'tls', sip_tls_port: '5061'};

test('tab revisits keep the SIP draft and do not issue another GET', async () => {
  const f = fixture();
  const first = f.context.loadSip();
  assert.equal(f.context.loadSip(), first);
  assert.equal(f.requests.length, 1);
  f.requests[0].resolve(saved);
  await first;
  f.edit('sip_registrar', 'draft.example.test');
  f.edit('sip_password', 'fixture-secret-draft');
  await f.context.loadSip();
  assert.equal(f.requests.length, 1);
  assert.equal(f.elements['sip-sip_registrar'].value, 'draft.example.test');
  assert.equal(f.elements['sip-sip_password'].value, 'fixture-secret-draft');
  assert.match(source, /onclick="loadSip\(true\)">↻ Reset/);
});

test('a delayed initial GET fills untouched fields and preserves edited fields', async () => {
  const f = fixture();
  const pending = f.context.loadSip();
  f.edit('sip_registrar', 'draft.example.test');
  f.edit('sip_transport_mode', 'udp');
  f.requests[0].resolve(saved);
  await pending;
  assert.equal(f.elements['sip-sip_registrar'].value, 'draft.example.test');
  assert.equal(f.elements['sip-sip_username'].value, 'saved-user');
  assert.equal(f.elements['sip-sip_transport_mode'].value, 'udp');
  assert.equal(f.elements['sip-tls-port-label'].style.display, 'none');
  assert.match(f.elements['sip-status'].textContent, /edits are kept/);
});

test('only the newest explicit reset can replace a draft, and newer typing survives it', async () => {
  const f = fixture();
  const old = f.context.loadSip();
  f.edit('sip_registrar', 'older-draft');
  const latest = f.context.loadSip(true);
  f.edit('sip_username', 'newer-draft-user');
  f.requests[1].resolve({...saved, sip_registrar: 'reset.example.test'});
  await latest;
  f.requests[0].resolve({...saved, sip_registrar: 'stale.example.test'});
  await old;
  assert.equal(f.elements['sip-sip_registrar'].value, 'reset.example.test');
  assert.equal(f.elements['sip-sip_username'].value, 'newer-draft-user');
});

test('Save invalidates an older GET; failed saves keep the draft across tab visits', async () => {
  for (const fail of [false, true]) {
    const f = fixture();
    const load = f.context.loadSip();
    f.edit('sip_registrar', 'submitted.example.test');
    const save = f.context.saveSip();
    assert.equal(f.requests[1].method, 'PUT');
    assert.equal(f.requests[1].body.sip_registrar, 'submitted.example.test');
    f.edit('sip_registrar', 'newer-draft.example.test');
    if (fail) f.requests[1].reject(new Error('Fixture save failure'));
    else f.requests[1].resolve({ok: true});
    assert.equal(await save, !fail);
    f.requests[0].resolve(saved);
    await load;
    await f.context.loadSip();
    assert.equal(f.requests.length, 2);
    assert.equal(f.elements['sip-sip_registrar'].value, 'newer-draft.example.test');
    if (fail) assert.match(f.elements['sip-status'].textContent, /Fixture save failure/);
  }
});

test('a failed reset leaves the saved draft intact and can be retried', async () => {
  const f = fixture();
  let pending = f.context.loadSip();
  f.requests[0].resolve(saved);
  await pending;
  f.edit('sip_registrar', 'keep-me.example.test');
  pending = f.context.loadSip(true);
  f.requests[1].reject(new Error('Fixture GET failure'));
  await pending;
  assert.equal(f.elements['sip-sip_registrar'].value, 'keep-me.example.test');
  pending = f.context.loadSip(true);
  f.requests[2].resolve(saved);
  await pending;
  assert.equal(f.elements['sip-sip_registrar'].value, saved.sip_registrar);
});

test('automatic retry after a failed initial GET never discards earlier edits', async () => {
  const f = fixture();
  let pending = f.context.loadSip();
  f.edit('sip_registrar', 'keep-me.example.test');
  f.requests[0].reject(new Error('Initial GET failed'));
  await pending;
  pending = f.context.loadSip();
  f.requests[1].resolve(saved);
  await pending;
  assert.equal(f.elements['sip-sip_registrar'].value, 'keep-me.example.test');
  assert.equal(f.elements['sip-sip_username'].value, saved.sip_username);
});
