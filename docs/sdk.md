# Browser SDK

```html
<script async src="https://agg.example.com/agg.js" data-site="pk_…"></script>
```

A 3 KB script with no dependencies. It sends a `page_view` on every page (and SPA navigation) and the events you
track. That is all it does: what to track and which properties to send is up to you.

## Track events

```js
agg.track('sign_up', { plan: 'pro' });
agg.track('feature_used', { feature: 'export_pdf' });
agg.track('purchase', { order_id: 'A-1001', value: 120, currency: 'EUR', items: [{ id: '73', name: 'Shoe', category: 'shoes', price: 60, quantity: 2 }] }, 'A-1001');
```

- **Name**: normalized to snake_case (`'Sign Up'` → `sign_up`).
- **Properties**: any JSON object, up to 8 KB per event. Available in aggregates as `props.<field>`.
- **Id** (optional third argument): the server counts an event with the same id only once (48 hours), e.g. when the
  order confirmation page is reloaded.

Calls made before the script loads need a stub above the snippet:

```html
<script>window.agg=window.agg||{q:[],track:function(){this.q.push(["track"].concat([].slice.call(arguments)))}}</script>
```

## Delivery and retries

- Events are queued and sent in batches (1 second debounce, ≤ 50 events and ≤ 60 KB per request) as `text/plain`,
  so there is no CORS preflight.
- Network errors, `429` and `5xx` responses are retried with exponential backoff (1 s, 2 s, 4 s … 60 s); other `4xx`
  responses drop the batch. Coming back online retries immediately.
- The queue is kept in `localStorage` (up to 500 events), so events that could not be delivered are sent by the next
  page.
- When the page is hidden or closed, waiting events are handed to `navigator.sendBeacon`.

## Metadata

Every event carries `meta`, filled in for you:

| | |
|---|---|
| `meta.path` | page path, without query string |
| `meta.referrer` | host of the external referrer, on the first page view of a visit |
| `meta.language` | browser language |
| `meta.browser`, `meta.os`, `meta.device` | parsed on the server from the User-Agent: Chrome / Safari / Firefox / Edge / …, macOS / Windows / iOS / Android / …, desktop / mobile / tablet |
| `meta.bot` | `true` for crawlers and scripts; automatic page views from bots are not stored |
| `meta.country`, `meta.city` | looked up from the client IP when the server has `AGG_GEOIP_URL` set and the site enables it in Settings (country by default; the IP is not stored) |
| `meta.ip` | client IP, only if the site enables "Store the client IP" (off by default) |

Use them like props: group page views by `meta.path`, sign-ups by `meta.device`, filter with `meta.browser == "Safari"`.

## Page views

On by default (Settings → Page views). `page_view` is sent on load and on `history.pushState` / `popstate`
navigation when the path changes. Turn it off to send page views yourself with `agg.page()`, or not at all.

## Visitor id, privacy, consent

- **Visitor id** (on by default): a random id in `localStorage` (`agg_vid`), needed for distinct visitor counts.
  No cookies, no fingerprinting. Switch it off per site.
- **Privacy filter**: the server removes these property names at any depth before storing events:
  `email, e_mail, phone, phone_number, first_name, last_name, full_name, street, address, postal_code, zip_code,
  password, card_number, iban`. Add more per site in Settings.
- **Consent**: with "Wait for consent" on, nothing is sent or stored until your consent banner calls:

```js
agg.consent({ analytics: true });   // send queued and future events
agg.consent({ analytics: false });  // drop queued events, send nothing
```

## From a backend or another app

```sh
curl -X POST https://agg.example.com/e -H 'Content-Type: application/json' \
  -d '{"site":"pk_…","events":[{"name":"invoice_paid","id":"inv-42","props":{"amount":49}}]}'
```

More examples (dedupe ids, batches, CI) and notes on `Origin`, `visitorId` and the User-Agent:
[api.md](api.md#from-a-backend-a-script-or-ci-curl).

## Manual init

Instead of `data-site`:

```js
agg.init({ endpoint: 'https://agg.example.com', site: 'pk_…' });
// skip the /v1/config request:
agg.init({ endpoint: '…', site: '…', config: { visitorId: true, pageViews: false, requireConsent: false } });
```

API: `agg.track(name, props?, id?)`, `agg.page()`, `agg.consent({ analytics })`, `agg.flush()`.
