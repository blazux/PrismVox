const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../app/static/index.html'), 'utf8');
const script = source.slice(source.indexOf('const OUTBOUND_PAGE_SIZE'), source.indexOf('function buildCallRow'));

function fixture() {
  const elements = Object.fromEntries(['calls-body', 'calls-count', 'calls-range', 'calls-prev', 'calls-next']
    .map(id => [id, {textContent: '', innerHTML: '', disabled: false}]));
  const requests = [];
  const context = vm.createContext({
    document: {getElementById: id => elements[id]},
    buildCallRow: row => 'row:' + row.id,
    esc: value => String(value),
    api(method, url) {
      return new Promise((resolve, reject) => requests.push({method, url, resolve, reject}));
    },
  });
  vm.runInContext(script, context);
  return {elements, requests, context};
}

function page(offset, count, total = 205, pending = 150) {
  return {offset, total, pending_total: pending,
          items: Array.from({length: count}, (_, i) => ({id: offset + i + 1, status: 'pending'}))};
}

test('all queue pages are reachable and global pending is distinct from the visible range', async () => {
  const f = fixture();
  for (const [offset, count] of [[0, 100], [100, 100], [200, 5]]) {
    const pending = f.context.loadOutbound(offset);
    const request = f.requests.at(-1);
    assert.equal(request.url, `/api/calls?limit=100&offset=${offset}`);
    assert.equal(f.elements['calls-prev'].disabled, true);
    assert.equal(f.elements['calls-next'].disabled, true);
    request.resolve(page(offset, count));
    await pending;
    assert.equal(f.elements['calls-count'].textContent, '205 calls total — 150 pending');
    assert.equal(f.elements['calls-range'].textContent, `Showing ${offset + 1}–${offset + count} of 205`);
    assert.equal(f.elements['calls-prev'].disabled, offset === 0);
    assert.equal(f.elements['calls-next'].disabled, offset === 200);
  }
  const refresh = f.context.loadOutbound();
  assert.equal(f.requests.at(-1).url, '/api/calls?limit=100&offset=200');
  f.requests.at(-1).resolve(page(200, 5));
  await refresh;
});

test('older successful and failed responses cannot replace the latest queue page', async () => {
  for (const fail of [false, true]) {
    const f = fixture();
    const older = f.context.loadOutbound(0);
    const latest = f.context.loadOutbound(100);
    f.requests[1].resolve(page(100, 100));
    await latest;
    if (fail) f.requests[0].reject(new Error('Stale error'));
    else f.requests[0].resolve(page(0, 100, 900, 800));
    await older;
    assert.equal(f.elements['calls-count'].textContent, '205 calls total — 150 pending');
    assert.equal(f.elements['calls-range'].textContent, 'Showing 101–200 of 205');
    assert.match(f.elements['calls-body'].innerHTML, /^row:101/);
    assert.equal(f.elements['calls-next'].disabled, false);
  }
});

test('a page removed by concurrent changes returns to the remaining last page', async () => {
  const f = fixture();
  const load = f.context.loadOutbound(200);
  f.requests[0].resolve(page(200, 0, 101, 5));
  await new Promise(setImmediate);
  assert.equal(f.requests.length, 2);
  assert.equal(f.requests[1].url, '/api/calls?limit=100&offset=100');
  f.requests[1].resolve(page(100, 1, 101, 5));
  await load;
  assert.equal(f.elements['calls-range'].textContent, 'Showing 101–101 of 101');
  assert.equal(f.elements['calls-prev'].disabled, false);
  assert.equal(f.elements['calls-next'].disabled, true);
});

test('empty queues and page errors have explicit ranges and recover on refresh', async () => {
  const f = fixture();
  let load = f.context.loadOutbound();
  f.requests[0].resolve(page(0, 0, 0, 0));
  await load;
  assert.equal(f.elements['calls-range'].textContent, 'Showing 0 of 0');
  assert.equal(f.elements['calls-prev'].disabled, true);
  assert.equal(f.elements['calls-next'].disabled, true);
  load = f.context.loadOutbound();
  f.requests[1].reject(new Error('Fixture failure'));
  await load;
  assert.match(f.elements['calls-body'].innerHTML, /Fixture failure/);
  assert.equal(f.elements['calls-range'].textContent, 'This page could not be loaded.');
  load = f.context.loadOutbound();
  f.requests[2].resolve(page(0, 1, 1, 1));
  await load;
  assert.equal(f.elements['calls-range'].textContent, 'Showing 1–1 of 1');
});
