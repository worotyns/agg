#!/usr/bin/env node
// Sends realistic traffic of a small shop with an app (page views, product views, add to cart, purchases,
// sign-ups, feature usage) to an agg server, the same way the browser SDK does.
//
//   node scripts/demo-traffic.mjs --url http://localhost:8080 --site pk_xxx [--visitors 200] [--live]
//
// Without --live it sends one burst of visits; with --live it keeps sending a few visits per second.
// Purchases carry an email on purpose so you can see the server-side privacy filter remove it.

const args = Object.fromEntries(process.argv.slice(2).map((a, i, all) => a.startsWith('--') ? [a.slice(2), all[i + 1] && !all[i + 1].startsWith('--') ? all[i + 1] : true] : null).filter(Boolean));
const url = (args.url || 'http://localhost:8080').replace(/\/$/, '');
const site = args.site;
if (!site) {
  console.error('usage: node scripts/demo-traffic.mjs --url http://localhost:8080 --site pk_xxx [--visitors 200] [--live]');
  process.exit(2);
}

const CATALOG = [
  ['101', 'Trail running shoe', 'shoes', 129], ['102', 'Road running shoe', 'shoes', 109], ['103', 'Hiking boot', 'shoes', 179],
  ['201', 'Merino socks', 'socks', 15], ['202', 'Compression socks', 'socks', 22],
  ['301', 'Rain jacket', 'jackets', 149], ['302', 'Down jacket', 'jackets', 229], ['303', 'Wind shell', 'jackets', 89],
  ['401', 'Running cap', 'accessories', 19], ['402', 'Headlamp', 'accessories', 39], ['403', 'Water bottle', 'accessories', 12],
];
const REFERRERS = ['www.google.com', 'www.google.com', 'www.facebook.com', 'duckduckgo.com', 'newsletter.example.com'];
// Popularity skew so top lists look like a real shop.
const weights = CATALOG.map((_, i) => 1 / (i + 1.5));
const pick = (arr, w) => {
  if (!w) return arr[Math.floor(Math.random() * arr.length)];
  let r = Math.random() * w.reduce((a, b) => a + b, 0);
  for (let i = 0; i < arr.length; i++) if ((r -= w[i]) <= 0) return arr[i];
  return arr[arr.length - 1];
};
const item = ([id, name, category, price], quantity = 1) => ({ id, name, category, price, quantity });
const AGENTS = [
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0',
  'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1',
  'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0 Mobile Safari/537.36',
  'Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0',
];
const PLANS = ['free', 'free', 'free', 'pro', 'team'];
const FEATURES = ['export_pdf', 'share_link', 'dark_mode', 'import_csv'];

function visit() {
  const visitorId = 'demo-' + Math.random().toString(36).slice(2, 10);
  const ua = pick(AGENTS);
  const language = pick(['en-US', 'pl-PL', 'de-DE']);
  const events = [];
  let path = '/';
  const ev = (name, props, id, extra) => events.push({ name, ts: Date.now(), visitorId, meta: { path, language, ...extra }, ...(props ? { props } : {}), ...(id ? { id } : {}) });
  const view = (p, extra) => { path = p; ev('page_view', null, null, extra); };
  const ref = Math.random() < 0.6 ? pick(REFERRERS) : undefined;
  view('/', ref ? { referrer: ref } : {});
  const cart = [];
  const views = 1 + Math.floor(Math.random() * 4);
  for (let i = 0; i < views; i++) {
    const p = pick(CATALOG, weights);
    view(`/p/${p[0]}`);
    ev('product_view', { id: p[0], name: p[1], category: p[2], price: p[3] });
    if (Math.random() < 0.3) {
      const q = Math.random() < 0.8 ? 1 : 2;
      cart.push(item(p, q));
      ev('add_to_cart', item(p, q));
    }
  }
  if (cart.length && Math.random() < 0.55) {
    const order = 'A-' + Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
    view('/checkout');
    ev('purchase', { order_id: order, currency: 'EUR', value: cart.reduce((s, i) => s + i.price * i.quantity, 0), items: cart,
      email: 'jane@example.com' }, order); // the email is removed by the server
  }
  if (Math.random() < 0.15) {
    view('/signup');
    ev('sign_up', { plan: pick(PLANS) });
    for (let i = 0; i < 1 + Math.floor(Math.random() * 3); i++) ev('feature_used', { feature: pick(FEATURES) });
  }
  return { ua, events };
}

async function send(events, ua) {
  const res = await fetch(url + '/e', { method: 'POST', headers: { 'Content-Type': 'text/plain', 'User-Agent': ua }, body: JSON.stringify({ site, events }) });
  if (!res.ok) throw new Error(`HTTP ${res.status}: ${await res.text()}`);
  return res.json();
}

async function burst(n) {
  // one request per visit, like a browser; a few in parallel
  let total = 0;
  for (let i = 0; i < n; i += 8) {
    const visits = Array.from({ length: Math.min(8, n - i) }, visit);
    const res = await Promise.all(visits.map((v) => send(v.events, v.ua)));
    total += res.reduce((s, r) => s + r.accepted, 0);
    await new Promise((r) => setTimeout(r, 40));
  }
  return total;
}

if (args.live) {
  console.log(`Sending live traffic to ${url} (Ctrl+C to stop)`);
  let n = 0;
  setInterval(async () => {
    try { n += await burst(1 + Math.floor(Math.random() * 3)); process.stdout.write(`\r${n} events sent`); } catch (e) { console.error('\n' + e.message); }
  }, 1000);
} else {
  const visitors = parseInt(args.visitors || '200', 10);
  burst(visitors).then((n) => console.log(`${visitors} visits, ${n} events accepted by ${url}`), (e) => { console.error(e.message); process.exit(1); });
}
