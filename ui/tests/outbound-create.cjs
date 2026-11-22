const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '../app/static/index.html'), 'utf8');
const script = source.slice(source.indexOf('let outboundCreateBusy'), source.indexOf('async function cancelOutbound'));

function fixture() {
  const fields = Object.fromEntries(Object.entries({
    'call-phone': '0596123456', 'call-mission': 'Confirm fixture appointment',
    'call-contact': 'Fixture person', 'call-attempts': '3', 'call-when': '',
  }).map(([id, value]) => [id, {value, disabled: false}]));
  const retry = {hidden: true, disabled: false};
  const checked = {hidden: true, disabled: false};
  const submit = {disabled: false};
  let resets = 0, refreshes = 0, serial = 0;
  const confirmations = [];
  const form = {elements: [...Object.values(fields), submit, retry, checked], reset() {resets++;}};
  const elements = {...fields, 'retry-call-request': retry, 'checked-call-request': checked, 'new-call-status': {textContent: ''},
                    'new-call-form': form, 'new-call-details': {open: true}};
  const requests = [];
  const context = vm.createContext({
    document: {getElementById: id => elements[id]}, get: id => fields[id].value,
    crypto: {randomUUID: () => 'fixture-id-' + ++serial}, Uint8Array,
    confirm: message => {confirmations.push(message); return true;},
    loadOutbound: () => {refreshes++;},
    api(method, url, body) {
      return new Promise((resolve, reject) => requests.push({method, url, body: JSON.parse(JSON.stringify(body)), resolve, reject}));
    },
  });
  vm.runInContext(script, context);
  return {context, fields, retry, checked, form, elements, requests, submit, confirmations,
          create: () => context.createOutboundCall({preventDefault() {}}),
          resubmit: () => context.submitPendingOutboundCall(),
          resets: () => resets, refreshes: () => refreshes};
}

test('a delayed POST freezes fields and ignores double submission', async () => {
  const f = fixture();
  const pending = f.create();
  assert.equal(f.requests.length, 1);
  assert.ok(f.form.elements.every(el => el.disabled));
  await f.create();
  await f.resubmit();
  assert.equal(f.requests.length, 1);
  f.requests[0].resolve({id: 42});
  await pending;
  assert.ok(f.form.elements.every(el => !el.disabled));
  assert.equal(f.resets(), 1);
  assert.equal(f.refreshes(), 1);
  assert.equal(f.elements['new-call-details'].open, false);
  assert.match(f.elements['new-call-status'].textContent, /request #42 confirmed/);
});

test('lost acknowledgement preserves draft and retries the exact original receipt', async () => {
  const f = fixture();
  let pending = f.create();
  const original = f.requests[0].body;
  f.requests[0].reject(new Error('Network unavailable'));
  await pending;
  assert.equal(f.resets(), 0);
  assert.equal(f.elements['new-call-details'].open, true);
  assert.equal(f.fields['call-mission'].value, 'Confirm fixture appointment');
  assert.equal(f.retry.hidden, false);
  assert.ok(f.form.elements.every(el => !el.disabled));
  pending = f.create();
  assert.deepEqual(f.requests[1].body, original);
  f.requests[1].resolve({id: 42});
  await pending;
  assert.equal(f.resets(), 1);
});

test('editing after an uncertain reply cannot create a second mission, and recovery keeps edits', async () => {
  const f = fixture();
  let pending = f.create();
  f.requests[0].reject(new Error('Connection reset'));
  await pending;
  f.fields['call-mission'].value = 'Edited mission';
  await f.create();
  assert.equal(f.requests.length, 1);
  assert.match(f.elements['new-call-status'].textContent, /previous request is not confirmed/);
  pending = f.resubmit();
  assert.deepEqual(f.requests[1].body, f.requests[0].body);
  f.requests[1].resolve({id: 42});
  await pending;
  assert.equal(f.resets(), 0);
  assert.equal(f.fields['call-mission'].value, 'Edited mission');
  assert.equal(f.elements['new-call-details'].open, true);
  assert.match(f.elements['new-call-status'].textContent, /edited mission is still a draft/);
  pending = f.create();
  assert.notEqual(f.requests[2].body.request_id, f.requests[0].body.request_id);
  assert.equal(f.requests[2].body.mission, 'Edited mission');
  f.requests[2].resolve({id: 43});
  await pending;
});

test('HTTP validation preserves the form while allowing a corrected creation intent', async () => {
  for (const status of [400, 422]) {
    const f = fixture();
    let pending = f.create();
    f.requests[0].reject(Object.assign(new Error('Invalid request'), {status}));
    await pending;
    assert.equal(f.resets(), 0);
    assert.equal(f.retry.hidden, true);
    assert.equal(f.fields['call-mission'].value, 'Confirm fixture appointment');
    f.fields['call-mission'].value = 'Corrected mission';
    pending = f.create();
    assert.notEqual(f.requests[1].body.request_id, f.requests[0].body.request_id);
    f.requests[1].resolve({id: 44});
    await pending;
  }
});

test('server failures and conflicts retain the original receipt without resetting controls', async () => {
  for (const status of [409, 500, 503]) {
    const f = fixture();
    f.fields['call-contact'].disabled = true;
    let pending = f.create();
    f.requests[0].reject(Object.assign(new Error('Request failed'), {status}));
    await pending;
    assert.equal(f.resets(), 0);
    assert.equal(f.retry.hidden, false);
    assert.equal(f.fields['call-contact'].disabled, true);
    assert.equal(f.submit.disabled, false);
    pending = f.resubmit();
    assert.deepEqual(f.requests[1].body, f.requests[0].body);
    f.requests[1].resolve({id: 45});
    await pending;
  }
});

test('HTTP consoles without randomUUID still create a stable cryptographic request key', async () => {
  const f = fixture();
  f.context.crypto.randomUUID = undefined;
  f.context.crypto.getRandomValues = bytes => {bytes.fill(17); return bytes;};
  let pending = f.create();
  assert.equal(f.requests[0].body.request_id, '11'.repeat(16));
  f.requests[0].reject(new Error('Lost acknowledgement'));
  await pending;
  pending = f.resubmit();
  assert.deepEqual(f.requests[1].body, f.requests[0].body);
  f.requests[1].resolve({id: 46});
  await pending;
});

test('an explicitly verified previous request can be left unchanged to schedule a separate draft', async () => {
  const f = fixture();
  let pending = f.create();
  f.requests[0].reject(new Error('Lost acknowledgement'));
  await pending;
  assert.equal(f.checked.hidden, false);
  f.fields['call-mission'].value = 'Separate mission';
  f.context.confirm = () => false;
  f.context.acknowledgePreviousOutboundCall();
  await f.create();
  assert.equal(f.requests.length, 1);
  assert.equal(f.checked.hidden, false);
  f.context.confirm = message => {f.confirmations.push(message); return true;};
  f.context.acknowledgePreviousOutboundCall();
  assert.equal(f.requests.length, 1, 'Acknowledgement never schedules or cancels a call');
  assert.match(f.confirmations[0], /may already exist/);
  assert.match(f.confirmations[0], /keeps it/);
  assert.equal(f.fields['call-mission'].value, 'Separate mission');
  assert.equal(f.checked.hidden, true);
  assert.equal(f.retry.hidden, true);
  assert.match(f.elements['new-call-status'].textContent, /previous request is unchanged/);
  pending = f.create();
  assert.equal(f.requests[1].body.mission, 'Separate mission');
  assert.notEqual(f.requests[1].body.request_id, f.requests[0].body.request_id);
  f.requests[1].resolve({id: 47});
  await pending;
});

test('the verification escape cannot clear a request while its POST is still in flight', async () => {
  const f = fixture();
  const pending = f.create();
  f.context.acknowledgePreviousOutboundCall();
  assert.equal(f.confirmations.length, 0);
  f.requests[0].reject(new Error('Lost acknowledgement'));
  await pending;
  const retry = f.resubmit();
  assert.deepEqual(f.requests[1].body, f.requests[0].body);
  f.requests[1].resolve({id: 48});
  await retry;
});
