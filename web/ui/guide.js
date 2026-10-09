import { html, CodeBox } from './lib.js';

// ---- tracking guide: the JavaScript API and how to design events, with this site's key and URL filled in

const SECTIONS = [
  ['install', 'Install'],
  ['api', 'JavaScript API'],
  ['design', 'Designing events'],
  ['meta', 'Automatic metadata'],
  ['aggregates', 'From events to numbers'],
  ['recipes', 'Recipes'],
  ['delivery', 'Delivery and limits'],
  ['privacy', 'Privacy and consent'],
  ['http', 'Backends, scripts, CI'],
  ['verify', 'Checking your events'],
];

const API = [
  ['agg.track(name, props?, id?)', html`Records an event. <code>name</code> is normalized to snake_case. <code>props</code> is any JSON object (≤ 8 KB). <code>id</code> is optional: an event with the same id is counted once
    (kept 48 h), e.g. an order id on a confirmation page that may be reloaded.`],
  ['agg.page()', html`Sends a <code>page_view</code> for the current path, unless the path has not changed. Called
    automatically on load and on SPA navigation when automatic page views are on (Settings).`],
  ['agg.consent({ analytics })', html`With “Wait for consent” on: <code>true</code> starts sending queued and future
    events; <code>false</code> drops the queue and sends nothing.`],
  ['agg.flush()', html`Sends queued events now instead of after the 1 s debounce. Rarely needed: the SDK also flushes
    with <code>sendBeacon</code> when the page is hidden.`],
  ['agg.init({ endpoint, site, config? })', html`Manual start when you load the script without <code>data-site</code> (e.g. from a bundle). <code>config</code> skips the settings request: <code>{ pageViews, visitorId, requireConsent }</code>.`],
  ['agg.version', html`SDK protocol version.`],
];

const NAMES = [
  ['Sign Up', 'sign_up'], ['addToCart', 'add_to_cart'], ['checkout-started', 'checkout_started'], ['Video: Play', 'video_play'],
];

export function GuidePage({ site, meta, base }) {
  const url = meta.baseUrl;
  const key = site.publicKey;
  // A server sends no Origin by itself; when the site restricts origins, show one that passes (https://*.x → https://www.x).
  const allowed = site.config.allowedOrigins || [];
  const origin = (allowed.find((o) => !o.includes('*')) || allowed[0] || '').replace('://*.', '://www.');
  const originHdr = origin ? ` \\\n  -H 'Origin: ${origin}'` : '';
  return html`
    <div class="page-head">
      <div><h1>Tracking guide</h1><div class="sub">The JavaScript API and how to design events for ${site.name}.
        Snippets below already contain this site's key.</div></div>
    </div>
    <div class="guide">
      <nav class="guide-toc card">
        ${SECTIONS.map(([id, label]) => html`<a href=${`#${base}/guide`} onClick=${(e) => { e.preventDefault(); document.getElementById('g-' + id).scrollIntoView({ behavior: 'smooth' }); }}>${label}</a>`)}
      </nav>
      <div class="guide-body">

        <section class="card" id="g-install">
          <h2>Install</h2>
          <p>Paste this before <code>${'</head>'}</code> on every page. It loads asynchronously and never blocks rendering.</p>
          <${CodeBox} text=${`<script async src="${url}/agg.js" data-site="${key}"></script>`} />
          <p class="hint">If you call <code>agg.track()</code> before the script has loaded (inline scripts, early clicks), put this
            stub above the snippet. Calls are queued and replayed in order:</p>
          <${CodeBox} text=${'<script>window.agg=window.agg||{q:[],track:function(){this.q.push(["track"].concat([].slice.call(arguments)))}}</script>'} />
          <p class="hint">Bundled apps can load the script themselves and start it manually:</p>
          <${CodeBox} text=${`agg.init({ endpoint: '${url}', site: '${key}' })`} />
        </section>

        <section class="card" id="g-api">
          <h2>JavaScript API</h2>
          <table class="api-table"><tbody>
            ${API.map(([sig, text]) => html`<tr><td class="mono nowrap">${sig}</td><td>${text}</td></tr>`)}
          </tbody></table>
          <${CodeBox} text=${`agg.track('sign_up', { plan: 'pro', source: 'pricing_page' })
agg.track('feature_used', { feature: 'export_pdf' })
agg.track('purchase', { order_id: 'A-1001', value: 258, currency: 'EUR' }, 'A-1001')`} />
        </section>

        <section class="card" id="g-design">
          <h2>Designing events</h2>
          <h3>Name: what happened</h3>
          <p>Use one name per user action, written as <b>object + verb</b> or a past-tense verb: <code>sign_up</code>, <code>add_to_cart</code>, <code>purchase</code>, <code>feature_used</code>, <code>video_play</code>. Keep the
            set small and stable: put variations into properties, not into names
            (<code>feature_used</code> with <code>{ feature: 'export_pdf' }</code>, not <code>export_pdf_used</code>).</p>
          <p>Names are normalized on the client and the server (lowercase snake_case, up to 64 characters), so these are
            the same event:</p>
          <div class="table-wrap"><table class="compact"><thead><tr><th>You send</th><th>Stored as</th></tr></thead><tbody>
            ${NAMES.map(([a, b]) => html`<tr><td class="mono">'${a}'</td><td class="mono">${b}</td></tr>`)}
          </tbody></table></div>

          <h3 style="margin-top:16px">Properties: the details you will group or sum by</h3>
          <ul class="tight">
            <li>A JSON object, up to <b>8 KB</b> per event. Nesting is fine: <code>props.customer.plan</code>.</li>
            <li>Send numbers as numbers (<code>value: 49.9</code>). Numeric strings also work in sums, but numbers are clearer.</li>
            <li>Use <b>stable ids</b> for things you will group by (<code>product_id: '73'</code>) and send a readable name next to
              them (<code>name: 'Trail shoe'</code>): aggregates can show the name as the label of the id.</li>
            <li>Put repeated things in an <b>array</b> (<code>items: [{ id, price, quantity }]</code>). An aggregate can
              “explode” it and count or sum each element.</li>
            <li>Keep the same property names across events (<code>value</code>, <code>currency</code>, <code>plan</code>), so one
              filter works everywhere.</li>
            <li>Do not send personal data. Fields such as <code>email</code> or <code>phone</code> are removed anyway (see Privacy).</li>
          </ul>

          <h3 style="margin-top:16px">Id: count once</h3>
          <p>Pass an id as the third argument when the same event can be sent twice: an order confirmation page that is reloaded,
            a webhook retried by your backend. The server keeps ids for 48 hours and ignores repeats.</p>
        </section>

        <section class="card" id="g-meta">
          <h2>Automatic metadata</h2>
          <p>Every event gets <code>meta</code>; you do not send it yourself.</p>
          <div class="table-wrap"><table class="compact"><tbody>
            <tr><td class="mono">meta.path</td><td>page path without the query string</td></tr>
            <tr><td class="mono">meta.referrer</td><td>external referrer host, on the first page view of a visit</td></tr>
            <tr><td class="mono">meta.language</td><td>browser language</td></tr>
            <tr><td class="mono">meta.browser, meta.os, meta.device</td><td>parsed on the server from the User-Agent (desktop / mobile / tablet)</td></tr>
            <tr><td class="mono">meta.country, meta.city</td><td>ISO country code, and city when chosen, looked up from the client IP when GeoIP is configured on the server and enabled in Settings</td></tr>
            <tr><td class="mono">meta.bot</td><td><code>true</code> for crawlers and scripts; automatic page views from bots are not stored</td></tr>
            <tr><td class="mono">meta.ip</td><td>client IP, only when “Store the client IP” is on in Settings</td></tr>
            <tr><td class="mono">visitor</td><td>random id from <code>localStorage</code> (when the visitor id is on); used for distinct counts</td></tr>
          </tbody></table></div>
        </section>

        <section class="card" id="g-aggregates">
          <h2>From events to numbers</h2>
          <p>Events alone are not shown anywhere except on the Events page. To get a number, create an <a href=${`#${base}/aggregates/new`}>aggregate</a>: it listens to event names and keeps a count, sum, distinct count or
            last value over sliding windows (5m … 30d). Its expressions see:</p>
          <div class="table-wrap"><table class="compact"><tbody>
            <tr><td class="mono">event</td><td>the event name</td></tr>
            <tr><td class="mono">props.…</td><td>your properties: <code>props.plan</code>, <code>props.customer.country</code></td></tr>
            <tr><td class="mono">meta.…</td><td>the metadata above: <code>meta.path</code>, <code>meta.device</code></td></tr>
            <tr><td class="mono">item.…</td><td>the current array element when the aggregate explodes an array (<code>props.items</code>)</td></tr>
            <tr><td class="mono">visitor, ts</td><td>visitor id; time the server received the event (unix seconds)</td></tr>
          </tbody></table></div>
          <p style="margin-top:12px">Examples for the events above:</p>
          <div class="table-wrap"><table class="compact"><thead><tr><th>You want</th><th>Aggregate</th></tr></thead><tbody>
            <tr><td>Sign-ups per plan</td><td>events <code>sign_up</code> · count · group by <code>props.plan</code></td></tr>
            <tr><td>Revenue</td><td>events <code>purchase</code> · sum of <code>props.value</code> · where <code>props.currency == "EUR"</code></td></tr>
            <tr><td>Bestsellers</td><td>events <code>purchase</code> · explode <code>props.items</code> · sum of <code>item.quantity ?? 1</code> · group by <code>item.id</code>, label <code>item.name</code></td></tr>
            <tr><td>Unique visitors</td><td>events <code>page_view</code> · count distinct (visitor)</td></tr>
            <tr><td>Last order time</td><td>events <code>purchase</code> · last timestamp</td></tr>
          </tbody></table></div>
          <p class="hint">Combine aggregates with <a href=${`#${base}/formulas`}>formulas</a> (<code>revenue_24h / purchases_24h</code>),
            watch them with <a href=${`#${base}/alerts`}>alerts</a>. Changed a definition? Rebuild replays the stored raw events
            (${meta.rawRetentionDays} days).</p>
        </section>

        <section class="card" id="g-recipes">
          <h2>Recipes</h2>
          <h3>Shop</h3>
          <${CodeBox} text=${`// product page
agg.track('product_view', { id: '73', name: 'Trail shoe', category: 'shoes', price: 129 })
// add to cart
agg.track('add_to_cart', { id: '73', name: 'Trail shoe', category: 'shoes', price: 129, quantity: 1 })
// order confirmation: the order id as the third argument counts a reloaded page once
agg.track('purchase', {
  order_id: 'A-1001', value: 258, currency: 'EUR',
  items: [{ id: '73', name: 'Trail shoe', category: 'shoes', price: 129, quantity: 2 }]
}, 'A-1001')`} />
          <h3 style="margin-top:14px">SaaS</h3>
          <${CodeBox} text=${`agg.track('sign_up', { plan: 'trial', source: 'pricing_page' })
agg.track('feature_used', { feature: 'export_pdf', plan: 'pro' })
agg.track('subscription_started', { plan: 'pro', value: 49, currency: 'EUR' }, subscriptionId)
agg.track('subscription_cancelled', { plan: 'pro', reason: 'too_expensive' }, subscriptionId)`} />
          <h3 style="margin-top:14px">Content and UI</h3>
          <${CodeBox} text=${`agg.track('article_read', { slug: 'sqlite-wal', category: 'engineering', percent: 75 })
agg.track('newsletter_subscribe', { placement: 'footer' })
document.querySelector('#cta').addEventListener('click', () => agg.track('cta_click', { cta: 'hero' }))`} />
        </section>

        <section class="card" id="g-delivery">
          <h2>Delivery and limits</h2>
          <ul class="tight">
            <li>Events are queued and sent in batches after a 1 s pause (≤ 50 events, ≤ 60 KB per request), as <code>text/plain</code>, so there is no CORS preflight.</li>
            <li>Network errors, <code>429</code> and <code>5xx</code> are retried with exponential backoff up to 60 s; other <code>4xx</code> responses drop the batch.</li>
            <li>The queue survives reloads in <code>localStorage</code> (≤ 500 events); undelivered events are sent by the next page.
              When the page is hidden, waiting events go out with <code>sendBeacon</code>.</li>
            <li>Server limits: props ≤ 8 KB per event (larger events are dropped), ≤ 100 events per request,
              ≤ 300 events per minute per visitor, 50 requests per second per IP.</li>
            <li>Allowed origins (Settings) restrict which websites may send events with this public key.</li>
          </ul>
        </section>

        <section class="card" id="g-privacy">
          <h2>Privacy and consent</h2>
          <p>Property names below are removed on the server at any depth before anything is stored. Add your own in Settings.</p>
          <div style="margin-bottom:10px">${meta.baseBlockedFields.map((f) => html`<span class="chip locked mono">${f}</span>`)}
            ${(site.config.blockedFields || []).map((f) => html`<span class="chip mono">${f}</span>`)}</div>
          <p>No cookies and no fingerprinting. The visitor id is a random value in <code>localStorage</code> and can be turned off.
            With “Wait for consent” on, nothing is sent or stored until your consent banner calls:</p>
          <${CodeBox} text=${`agg.consent({ analytics: true })   // start sending
agg.consent({ analytics: false })  // drop the queue, send nothing`} />
        </section>

        <section class="card" id="g-http">
          <h2>Backends, scripts, CI</h2>
          <p>Anything that can make an HTTP request can send events: your backend, a cron job, a CI pipeline, a webhook
            handler. Fields: <code>name</code> (required), <code>props</code>, <code>id</code> (dedupe), <code>visitorId</code>, <code>ts</code> (unix ms), <code>meta</code> (<code>path</code>, <code>referrer</code>, <code>language</code>).
            ${origin && html` This site restricts allowed origins, so the examples send <code>Origin: ${origin}</code>; without it the request gets <code>403</code>.`}</p>
          <${CodeBox} text=${`curl -X POST ${url}/e -H 'Content-Type: application/json'${originHdr} \\
  -d '{"site":"${key}","events":[{"name":"invoice_paid","id":"inv-42","props":{"amount":49,"currency":"EUR"}}]}'`} />
          <p class="hint">The response is <code>{"accepted": 1, "dropped": 0}</code>. Sending the same <code>id</code> again within 48 h
            returns <code>"dropped": 1</code>, so retried webhooks count once.</p>
          <h3 style="margin-top:14px">A deploy from CI, a nightly job</h3>
          <${CodeBox} text=${`curl -fsS -X POST ${url}/e -H 'Content-Type: application/json'${originHdr} -d @- <<JSON
{"site":"${key}","events":[
  {"name":"deploy_finished","id":"$GITHUB_SHA","props":{"service":"api","version":"$VERSION","duration_s":84}},
  {"name":"backup_finished","props":{"size_mb":512}}
]}
JSON`} />
          <ul class="tight">
            <li>Up to 100 events and 256 KB per request; retry <code>429</code> and <code>5xx</code> with a backoff.</li>
            <li>Send <code>visitorId</code> (e.g. your user id) when the event should count towards distinct visitors.</li>
            <li>Browser, OS and device come from the <code>User-Agent</code> header. curl and HTTP libraries are marked <code>meta.bot = true</code>; their events are still counted. Forward your user's User-Agent with <code>-A</code> to get their device.</li>
          </ul>
        </section>

        <section class="card" id="g-verify">
          <h2>Checking your events</h2>
          <ul class="tight">
            <li><a href=${`#${base}/events`}>Events → Live</a> shows events within seconds, after the privacy filter.</li>
            <li><a href=${`#${base}/events?mode=audit`}>Events → Audit</a> lists every stored event with filters by name and visitor,
              exact timestamps, paging to older events and a JSON download.</li>
            <li>The aggregate editor has a tester that runs a definition against recent events before you save it.</li>
          </ul>
        </section>
      </div>
    </div>`;
}
