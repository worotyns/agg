// Tests for the browser SDK. Run with: node --test 'web/sdk/*.test.mjs'
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const source = readFileSync(new URL('./agg.js', import.meta.url), 'utf8');
const CONFIG = { visitorId: true, pageViews: false, requireConsent: false };

// Loads the SDK into a fake browser. Timers run only when env.tick() is called; fetch answers with env.status.
function browser({ path = '/p/1', referrer = '', storage = {}, beacon = true } = {}) {
  const timers = [];
  const listeners = {};
  const requests = [];
  const beacons = [];
  const env = { status: 202, fail: false };
  const win = {
    location: { pathname: path, hostname: 'shop.example.com' },
    history: { pushState(_s, _t, url) { win.location.pathname = url.split('?')[0]; } },
    navigator: { language: 'pl-PL', sendBeacon: (url, blob) => { if (!beacon) return false; beacons.push(JSON.parse(blob.text)); return true; } },
    localStorage: { getItem: (k) => storage[k] ?? null, setItem: (k, v) => { storage[k] = String(v); } },
    crypto: { randomUUID: () => 'uuid-1' },
    addEventListener(t, f) { (listeners[t] ||= []).push(f); },
    console: { warn() {} },
    fetch(url, opts) {
      requests.push({ url, body: JSON.parse(opts.body) });
      return env.fail ? Promise.reject(new Error('offline')) : Promise.resolve({ ok: env.status < 300, status: env.status });
    },
  };
  const doc = { referrer, currentScript: null, visibilityState: 'visible', addEventListener(t, f) { (listeners[t] ||= []).push(f); } };
  const ctx = {
    window: win, document: doc, URL, JSON, Date, Math, Object, Array, String, encodeURIComponent, Promise,
    Blob: class { constructor(parts) { this.text = parts.join(''); } },
    setTimeout: (f, ms) => { timers.push({ f, ms }); return timers.length; }, clearTimeout: () => {},
  };
  vm.createContext(ctx);
  vm.runInContext(source, ctx);
  return Object.assign(env, {
    agg: win.agg, win, doc, requests, beacons, storage, listeners, timers,
    // runs pending timers and lets promises settle; returns the delays that were scheduled
    async tick() { const t = timers.splice(0); for (const x of t) x.f(); await new Promise((r) => setImmediate(r)); return t.map((x) => x.ms); },
    sent() { return requests.flatMap((r) => r.body.events); },
    init(config = {}) { win.agg.init({ site: 'pk_1', endpoint: 'https://agg.test/', config: { ...CONFIG, ...config } }); },
  });
}

const plain = (x) => JSON.parse(JSON.stringify(x));

test('normalizes event names like the server', () => {
  const { agg } = browser();
  assert.equal(agg._norm('Sign Up'), 'sign_up');
  assert.equal(agg._norm('signUp'), 'sign_up');
  assert.equal(agg._norm('  --feature-used!! '), 'feature_used');
});

test('track() batches events with props, id, visitor id and page meta', async () => {
  const env = browser({ path: '/pricing' });
  env.init();
  env.agg.track('Sign Up', { plan: 'pro' }, 'u-1');
  env.agg.track('feature_used', { feature: 'export' });
  assert.equal(env.requests.length, 0, 'debounced');
  await env.tick();
  assert.equal(env.requests.length, 1);
  assert.equal(env.requests[0].url, 'https://agg.test/e');
  assert.equal(env.requests[0].body.site, 'pk_1');
  const [a, b] = env.sent();
  assert.deepEqual(plain({ name: a.name, id: a.id, props: a.props, meta: a.meta, visitorId: a.visitorId }),
    { name: 'sign_up', id: 'u-1', props: { plan: 'pro' }, meta: { path: '/pricing', language: 'pl-PL' }, visitorId: 'uuid-1' });
  assert.equal(b.name, 'feature_used');
  assert.equal(env.storage.agg_vid, 'uuid-1');
});

test('retries with exponential backoff on network errors and 5xx, keeps order, then delivers', async () => {
  const env = browser();
  env.init();
  env.agg.track('a');
  env.fail = true;
  await env.tick(); // first attempt fails
  let delays = await env.tick(); // retry after 1 s, fails
  assert.deepEqual(delays, [1000]);
  env.fail = false;
  env.status = 503;
  env.agg.track('b');
  delays = await env.tick();
  assert.deepEqual(delays, [2000], 'backoff doubles');
  env.status = 202;
  await env.tick();
  assert.equal(env.requests.length, 4);
  assert.deepEqual(env.requests[3].body.events.map((e) => e.name), ['a', 'b']);
  assert.equal(env.storage.agg_q_pk_1, '[]', 'queue empty after delivery');
});

test('4xx other than 429 drops the batch instead of retrying forever', async () => {
  const env = browser();
  env.init();
  env.status = 400;
  env.agg.track('bad');
  await env.tick();
  assert.equal(env.timers.length, 0);
  assert.equal(env.storage.agg_q_pk_1, '[]');
  env.status = 429;
  env.agg.track('limited');
  await env.tick();
  assert.equal(env.timers.length, 1, '429 is retried');
});

test('undelivered events survive a reload and are sent by the next page', async () => {
  const storage = {};
  const first = browser({ storage, beacon: false });
  first.init();
  first.fail = true;
  first.agg.track('purchase', { value: 10 }, 'A-1');
  await first.tick();
  first.listeners.pagehide.forEach((f) => f()); // sendBeacon refused
  assert.equal(JSON.parse(storage.agg_q_pk_1).length, 1);

  const second = browser({ storage });
  second.init();
  await second.tick();
  assert.deepEqual(second.sent().map((e) => [e.name, e.id]), [['purchase', 'A-1']]);
});

test('page hide hands waiting events to sendBeacon', async () => {
  const env = browser();
  env.init();
  env.agg.track('a');
  env.agg.track('b');
  env.doc.visibilityState = 'hidden';
  env.listeners.visibilitychange.forEach((f) => f());
  assert.deepEqual(env.beacons[0].events.map((e) => e.name), ['a', 'b']);
  assert.equal(env.storage.agg_q_pk_1, '[]');
});

test('splits large queues into batches of at most 50 events and 60 KB', async () => {
  const env = browser();
  env.init();
  for (let i = 0; i < 120; i++) env.agg.track('e', { note: 'x'.repeat(1500) });
  for (let i = 0; i < 10 && env.timers.length; i++) await env.tick();
  assert.equal(env.sent().length, 120);
  assert.ok(env.requests.every((r) => r.body.events.length <= 50 && JSON.stringify(r.body).length < 61000));
});

test('page views: automatic, external referrer host only on the first, SPA navigation', async () => {
  const env = browser({ referrer: 'https://www.google.com/search?q=x' });
  env.init({ pageViews: true });
  env.win.history.pushState({}, '', '/p/2?q=1');
  await env.tick();
  await env.tick();
  assert.deepEqual(plain(env.sent().map((e) => [e.name, e.meta])), [
    ['page_view', { path: '/p/1', language: 'pl-PL', referrer: 'www.google.com' }],
    ['page_view', { path: '/p/2', language: 'pl-PL' }],
  ]);
});

test('requireConsent: nothing is sent or stored until consent; denial drops the queue', async () => {
  const env = browser();
  env.init({ requireConsent: true });
  env.agg.track('a');
  await env.tick();
  assert.equal(env.requests.length, 0);
  assert.equal(env.storage.agg_vid, undefined);
  env.agg.consent({ analytics: true });
  await env.tick();
  assert.deepEqual(env.sent().map((e) => e.name), ['a']);

  const denied = browser();
  denied.init({ requireConsent: true });
  denied.agg.track('a');
  denied.agg.consent({ analytics: false });
  denied.agg.track('b');
  await denied.tick();
  assert.equal(denied.requests.length, 0);
});

test('no visitor id when disabled; oversize props are dropped', async () => {
  const env = browser();
  env.init({ visitorId: false });
  env.agg.track('a');
  env.agg.track('big', { blob: 'x'.repeat(9000) });
  await env.tick();
  assert.deepEqual(env.sent().map((e) => e.name), ['a']);
  assert.ok(!('visitorId' in env.sent()[0]));
});

test('events tracked before init are kept', async () => {
  const env = browser();
  env.agg.track('early', { x: 1 });
  env.init();
  await env.tick();
  assert.deepEqual(env.sent().map((e) => e.name), ['early']);
});
