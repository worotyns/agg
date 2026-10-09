import { render } from './vendor/preact-htm.js';
import {
  html, useState, useEffect, api, useLoad, useRoute, go, Alert, CodeBox, ConfirmTyped, Seg, TagInput,
  compact, num, unitValue, lastValue, ago, slugify,
} from './lib.js';
import { Sparkline, TopList } from './charts.js';
import { AggregatesPage, AggregateEditor, AggregateDetail } from './aggregates.js';
import { FormulasPage, ExportsPage, ApiPage } from './more.js';
import { AlertsPage, AccessPage, Bell } from './alerts.js';
import { EventsPage } from './events.js';
import { GuidePage } from './guide.js';

// ---- login

function Login({ onDone }) {
  const [token, setToken] = useState('');
  const [err, setErr] = useState(null);
  const submit = async (e) => {
    e.preventDefault();
    setErr(null);
    try { await api('POST', '/login', { token }); onDone(); } catch (x) { setErr(x); }
  };
  return html`
    <div class="login">
      <div class="brand"><${Logo} /> agg</div>
      <p class="tagline">Real-time aggregates for your events</p>
      <form class="card" onSubmit=${submit}>
        <label class="field"><span>Admin token</span>
          <input type="password" value=${token} onInput=${(e) => setToken(e.target.value)} autofocus /></label>
        <${Alert} error=${err} />
        <button class="primary" style="width:100%;justify-content:center" disabled=${!token.trim()}>Log in</button>
        <p class="hint" style="margin-top:12px">The token is printed when the server starts for the first time.
          Lost it? Run <code>agg reset-admin-token</code>.</p>
      </form>
    </div>`;
}

const Logo = () => html`<svg width="18" height="18" viewBox="0 0 16 16" aria-hidden="true">
  <rect x="1" y="9" width="3" height="6" rx="1" fill="var(--accent)" /><rect x="6.5" y="5" width="3" height="10" rx="1" fill="var(--accent)" />
  <rect x="12" y="1" width="3" height="14" rx="1" fill="var(--accent)" /></svg>`;

// ---- shell

const NAV = [
  ['insights', 'Insights'], ['events', 'Events'], ['aggregates', 'Aggregates'], ['formulas', 'Formulas'],
  ['alerts', 'Alerts'], ['exports', 'Exports'], ['api', 'Values API'], ['guide', 'Tracking guide'], ['settings', 'Settings'],
];

function App() {
  const [authed, setAuthed] = useState(null);
  const [meta, setMeta] = useState(null);
  const [sites, setSites] = useState(null);
  const route = useRoute();

  const loadSites = () => api('GET', '/sites').then(setSites);
  useEffect(() => {
    api('GET', '/me').then(() => setAuthed(true), () => setAuthed(false));
    const out = () => setAuthed(false);
    window.addEventListener('agg:logout', out);
    return () => window.removeEventListener('agg:logout', out);
  }, []);
  useEffect(() => {
    if (authed) { api('GET', '/meta').then(setMeta); loadSites(); }
  }, [authed]);

  if (authed === null) return null;
  if (!authed) return html`<${Login} onDone=${() => setAuthed(true)} />`;
  if (!sites || !meta) return null;

  const [first, siteId, page, ...rest] = route.parts;
  const site = first === 's' && sites.find((s) => String(s.id) === siteId);
  if (first === 'access') {
    return html`<${Shell} sites=${sites} page="access"><${AccessPage} meta=${meta} /><//>`;
  }
  if (first === 'new-site' || !sites.length) {
    return html`<${Shell} sites=${sites}><${NewSite} first=${!sites.length} onCreated=${async (s) => { await loadSites(); go(`/s/${s.id}/settings?installed=0`); }} /><//>`;
  }
  if (!site) {
    const last = localStorage.getItem('agg_site');
    const s = sites.find((x) => String(x.id) === last) || sites[0];
    go(`/s/${s.id}/insights`);
    return null;
  }
  localStorage.setItem('agg_site', String(site.id));
  const ctx = { site, meta, base: `/s/${site.id}`, reloadSites: loadSites, query: route.query };
  let body;
  switch (page) {
    case 'events': body = html`<${EventsPage} ...${ctx} />`; break;
    case 'aggregates':
      if (rest[0] === 'new') body = html`<${AggregateEditor} ...${ctx} />`;
      else if (rest[0] && rest[1] === 'edit') body = html`<${AggregateEditor} ...${ctx} id=${rest[0]} />`;
      else if (rest[0]) body = html`<${AggregateDetail} ...${ctx} id=${rest[0]} />`;
      else body = html`<${AggregatesPage} ...${ctx} />`;
      break;
    case 'formulas': body = html`<${FormulasPage} ...${ctx} />`; break;
    case 'alerts': body = html`<${AlertsPage} ...${ctx} />`; break;
    case 'exports': body = html`<${ExportsPage} ...${ctx} />`; break;
    case 'api': body = html`<${ApiPage} ...${ctx} />`; break;
    case 'settings': body = html`<${SettingsPage} ...${ctx} />`; break;
    case 'guide': body = html`<${GuidePage} ...${ctx} />`; break;
    default: body = html`<${InsightsPage} ...${ctx} />`;
  }
  return html`<${Shell} sites=${sites} site=${site} page=${page || 'insights'}>${body}<//>`;
}

function Shell({ sites, site, page, children }) {
  const [theme, setTheme] = useState(() => { try { return localStorage.getItem('agg-theme') || ''; } catch (e) { return ''; } });
  useEffect(() => {
    if (theme) document.documentElement.dataset.theme = theme;
    else delete document.documentElement.dataset.theme;
  }, [theme]);
  const toggleTheme = () => {
    const next = (theme || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light')) === 'dark' ? 'light' : 'dark';
    setTheme(next);
    try { localStorage.setItem('agg-theme', next); } catch (e) {}
  };
  return html`
    <header class="topbar">
      <a class="brand" href="#/"><${Logo} /> agg</a>
      ${site && html`
        <select style="width:auto;max-width:180px" value=${site.id}
          onChange=${(e) => e.target.value === 'new' ? go('/new-site') : go(`/s/${e.target.value}/${page}`)}>
          ${sites.map((s) => html`<option value=${s.id}>${s.name}</option>`)}
          <option value="new">+ New site…</option>
        </select>
        <nav class="nav">${NAV.map(([k, label]) => html`
          <a class=${page === k ? 'active' : ''} href=${`#/s/${site.id}/${k}`}>${label}</a>`)}</nav>`}
      ${!site && html`<div class="spacer"></div>`}
      <div class="top-right">
        <button class="theme-btn" title="Toggle theme" aria-label="Toggle theme" onClick=${toggleTheme}>◐</button>
        <${Bell} />
        <a class=${page === 'access' ? 'active' : ''} href="#/access">API & MCP</a>
        <button class="link" onClick=${() => api('POST', '/logout').then(() => location.reload())}>Log out</button>
      </div>
    </header>
    <main class="main">${children}</main>`;
}

// ---- sites

function NewSite({ first, onCreated }) {
  const [name, setName] = useState('');
  const [slug, setSlug] = useState('');
  const [preset, setPreset] = useState('website');
  const [err, setErr] = useState(null);
  const presets = useLoad(() => api('GET', '/presets'), []);
  const submit = async (e) => {
    e.preventDefault();
    try {
      onCreated(await api('POST', '/sites', { name, slug: slug || slugify(name), preset, timezone: Intl.DateTimeFormat().resolvedOptions().timeZone }));
    } catch (x) { setErr(x); }
  };
  const chosen = (presets.data || []).find((p) => p.id === preset);
  return html`
    <div style="max-width:620px">
      <div class="page-head"><div><h1>${first ? 'Add your first site' : 'New site'}</h1>
        <div class="sub">A site is one website or app sending events. Data, aggregates, alerts and settings are per site.</div></div></div>
      <form class="card" onSubmit=${submit}>
        <label class="field"><span>Name</span>
          <input value=${name} placeholder="My shop" onInput=${(e) => setName(e.target.value)} autofocus /></label>
        <label class="field"><span>ID</span>
          <input class="mono" value=${slug} placeholder=${slugify(name) || 'my_shop'} onInput=${(e) => setSlug(e.target.value)} />
          <div class="hint">Used as the <code>site</code> label in Prometheus. Lowercase letters, digits and _.</div></label>
        <div class="field"><span class="field-label">Start with</span>
          ${(presets.data || []).map((p) => html`<label class="check"><input type="radio" name="preset" checked=${preset === p.id} onChange=${() => setPreset(p.id)} />
            <span><b>${p.title}</b><div class="muted small">${p.description}</div></span></label>`)}
          <label class="check"><input type="radio" name="preset" checked=${preset === 'none'} onChange=${() => setPreset('none')} />
            <span><b>Empty</b><div class="muted small">No aggregates; add your own.</div></span></label>
        </div>
        ${chosen && html`<details><summary>What it creates</summary><${PresetContents} p=${chosen} /></details>`}
        <${Alert} error=${err} />
        <button class="primary" disabled=${!name}>Create site</button>
      </form>
    </div>`;
}

function PresetContents({ p }) {
  return html`<div class="small">
    <div><b>Aggregates:</b> ${p.aggregates.map((a) => a.name).join(', ')}</div>
    ${p.formulas.length > 0 && html`<div><b>Formulas:</b> ${p.formulas.map((f) => f.name).join(', ')}</div>`}
    ${p.alerts.length > 0 && html`<div><b>Alerts:</b> ${p.alerts.map((a) => a.def.title).join('; ')}</div>`}
    <div style="margin-top:6px"><b>Your site sends:</b><ul style="margin:4px 0 0;padding-left:18px">${p.setup.map((x) => html`<li>${x}</li>`)}</ul></div>
  </div>`;
}

function PresetCard({ site, reloadSites }) {
  const presets = useLoad(() => api('GET', '/presets'), []);
  const [id, setId] = useState('shop');
  const [res, setRes] = useState(null);
  const [err, setErr] = useState(null);
  const chosen = (presets.data || []).find((p) => p.id === id);
  const apply = async () => {
    setErr(null);
    try {
      setRes(await api('POST', `/sites/${site.id}/presets/${id}`, { timezone: Intl.DateTimeFormat().resolvedOptions().timeZone }));
      reloadSites();
    } catch (e) { setErr(e); }
  };
  return html`
    <div class="card">
      <h2>Presets</h2>
      <p class="muted">Add a ready set of aggregates, formulas and alerts for a use case. Only missing names are added; nothing is changed or removed.</p>
      <div class="row" style="margin-bottom:10px">
        <select style="width:auto" value=${id} onChange=${(e) => { setId(e.target.value); setRes(null); }}>
          ${(presets.data || []).map((p) => html`<option value=${p.id}>${p.title}</option>`)}</select>
        <button onClick=${apply}>Apply preset</button>
      </div>
      ${chosen && html`<${PresetContents} p=${chosen} />`}
      <${Alert} error=${err} ok=${res && `Added: ${res.created.length ? res.created.join(', ') : 'nothing new'}.`} />
    </div>`;
}

// ---- insights

const WINDOWS = ['1h', '24h', '7d', '30d'];

function Delta({ change, prev, window }) {
  if (prev === undefined || prev === null) return null;
  if (change === null || change === undefined) {
    if ((prev === 0 || prev === '0') && change == null) return html`<div class="delta"><span class="up">● new</span><span class="faint">vs previous ${window}</span></div>`;
    return html`<div class="delta faint">previous ${window}: ${compact(prev)}</div>`;
  }
  const up = change >= 0;
  return html`<div class="delta"><span class=${up ? 'up' : 'down'}>${up ? '▲' : '▼'} ${Math.abs(change).toFixed(0)}%</span><span class="faint">vs prev. ${window} (${compact(prev)})</span></div>`;
}

function InsightsPage({ site, base, query }) {
  const [win, setWin] = useState(() => localStorage.getItem('agg_window') || '24h');
  const ins = useLoad(() => api('GET', `/sites/${site.id}/insights?window=${win}`), [site.id, win], 30000);
  const names = useLoad(() => api('GET', `/sites/${site.id}/event-names`), [site.id]);
  const setW = (w) => { localStorage.setItem('agg_window', w); setWin(w); };
  const d = ins.data;
  const noEvents = names.data && names.data.length === 0;
  return html`
    <div class="page-head">
      <div><h1>${site.name}</h1><div class="sub">Pinned aggregates and formulas · refreshes every 30 s</div></div>
      <${Seg} options=${WINDOWS} value=${win} onChange=${setW} />
    </div>
    <${Alert} error=${ins.error} />
    ${noEvents && html`<div class="alert warn">No events received in the last 24 hours. <a href=${`#${base}/settings`}>Install the snippet</a> on your site, then watch them arrive on <a href=${`#${base}/events`}>Events</a>.</div>`}
    ${d && html`
      <div class="grid tiles">
        ${d.tiles.map((t) => html`
          <a class="card tile" href=${`#${base}/aggregates/${t.id}`}>
            <div class="label">${t.title}</div>
            <div class="value">${t.op === 'last_timestamp' || t.op === 'last_value' ? lastValue(t, t.value) : compact(t.value)}</div>
            ${t.op === 'last_timestamp' || t.op === 'last_value'
              ? html`<div class="delta">latest</div>`
              : html`<${Delta} change=${t.change} prev=${t.prev} window=${d.window} />`}
            <${Sparkline} points=${t.series} />
          </a>`)}
        ${d.formulas.map((f) => html`
          <a class="card tile" href=${`#${base}/formulas`}>
            <div class="label">${f.title}</div>
            <div class="value">${unitValue(f.value, f.unit)}</div>
            <div class="delta">formula</div>
          </a>`)}
      </div>
      ${d.tops.length > 0 && html`
        <div class="masonry" style="margin-top:16px">
          ${d.tops.map((t) => html`
            <div class="card">
              <div class="row" style="margin-bottom:8px"><h2 style="margin:0">${t.title}</h2><span class="spacer"></span>
                <span class="faint small">top ${t.dimension} · ${d.window}</span></div>
              <${TopList} items=${t.items} format=${t.op === 'last_timestamp' ? ago : num} />
              <div class="small" style="margin-top:8px"><a href=${`#${base}/aggregates/${t.id}`}>Details →</a></div>
            </div>`)}
        </div>`}
      ${!d.tiles.length && !d.tops.length && !d.formulas.length && html`
        <div class="card empty">Nothing pinned yet. <a href=${`#${base}/aggregates/new`}>Create an aggregate</a> and keep “Show on Insights” on.</div>`}`}`;
}

// ---- settings

function SettingsPage({ site, meta, reloadSites, query }) {
  const [cfg, setCfg] = useState(site.config);
  const [name, setName] = useState(site.name);
  const [msg, setMsg] = useState(null);
  const [err, setErr] = useState(null);
  const [del, setDel] = useState(false);
  useEffect(() => { setCfg(site.config); setName(site.name); }, [site.id]);
  const set = (k, v) => setCfg({ ...cfg, [k]: v });
  const save = async () => {
    setErr(null);
    try { await api('PUT', `/sites/${site.id}`, { name, config: cfg }); await reloadSites(); setMsg('Saved. Browsers pick up changes within 5 minutes.'); }
    catch (e) { setErr(e); }
  };
  const snippet = `<script async src="${meta.baseUrl}/agg.js" data-site="${site.publicKey}"></script>`;
  return html`
    <div class="page-head">
      <div><h1>Settings</h1><div class="sub">Installation, privacy and presets.</div></div>
    </div>
    ${query.get('installed') === '0' && html`<div class="alert ok">Site created. Add the snippet below to every page, then open <a href=${`#/s/${site.id}/events`}>Events</a>.</div>`}
    <div class="card">
      <h2>Install</h2>
      <p class="muted">Paste before <code>${'</head>'}</code> on every page. The 3 KB script sends page views and the events
        you track; it batches them and retries when the network or the server is down.</p>
      <${CodeBox} text=${snippet} />
      <div class="field-label" style="margin-top:14px">Track your own events</div>
      <${CodeBox} text=${`agg.track('sign_up', { plan: 'pro' })
agg.track('feature_used', { feature: 'export_pdf' })
// optional third argument: a unique id, so a repeated event (e.g. a reloaded order page) counts once
agg.track('purchase', { order_id: 'A-1001', value: 120, currency: 'EUR' }, 'A-1001')`} />
      <p class="hint">Names become snake_case. Properties are yours (up to 8 KB per event). Every event also gets <code>meta</code>:
        path, referrer, language, plus browser, os and device from the User-Agent.
        The <a href=${`#/s/${site.id}/guide`}>tracking guide</a> covers the full JavaScript API and how to design events.</p>
      <details style="margin-top:8px"><summary>From a backend or another app</summary>
        <${CodeBox} text=${`curl -X POST ${meta.baseUrl}/e -H 'Content-Type: application/json' \\
  -d '{"site":"${site.publicKey}","events":[{"name":"invoice_paid","props":{"amount":49}}]}'`} />
        <p class="hint">Calls made before the script loads need this stub above the snippet: <code>${'<script>window.agg=window.agg||{q:[],track:function(){this.q.push(["track"].concat([].slice.call(arguments)))}}</script>'}</code></p>
      </details>
      <dl class="kv small" style="margin-top:12px"><dt>Public site key</dt><dd class="mono">${site.publicKey}</dd><dt>Site ID</dt><dd class="mono">${site.slug}</dd></dl>
    </div>

    <div class="card">
      <h2>Tracking</h2>
      <label class="field"><span>Name</span><input value=${name} onInput=${(e) => setName(e.target.value)} /></label>
      <fieldset><legend>Page views</legend>
        <label class="check"><input type="checkbox" checked=${cfg.pageViews} onChange=${(e) => set('pageViews', e.target.checked)} />
          <span>Send a <code>page_view</code> automatically on every page and SPA navigation (path without query string, external referrer host)</span></label>
      </fieldset>
      <fieldset><legend>Privacy</legend>
        <div class="field-label">Property names always removed on the server, at any depth</div>
        <div>${meta.baseBlockedFields.map((f) => html`<span class="chip locked mono">${f}</span>`)}</div>
        <div class="field-label" style="margin-top:8px">Also remove</div>
        <${TagInput} values=${cfg.blockedFields} onChange=${(v) => set('blockedFields', v)} placeholder="field name" mono />
        <label class="check"><input type="checkbox" checked=${cfg.visitorId} onChange=${(e) => set('visitorId', e.target.checked)} />
          <span>Visitor id: a random id in localStorage, needed to count distinct visitors. No cookies, no fingerprinting.</span></label>
        <label class="check"><input type="checkbox" checked=${cfg.collectIp} onChange=${(e) => set('collectIp', e.target.checked)} />
          <span>Store the client IP in <code>meta.ip</code> (personal data; off by default)</span></label>
        ${meta.geo?.enabled && html`<label class="field"><span>Location from IP</span>
          <select value=${cfg.geo || 'country'} onChange=${(e) => set('geo', e.target.value)}>
            <option value="off">Off</option><option value="country">Country</option><option value="city">City</option></select>
          <div class="hint">Looked up by your GeoIP service; the IP itself is not stored unless the option above is on.
            This product includes GeoLite2 data created by MaxMind, available from <a href="https://www.maxmind.com" target="_blank" rel="noopener">https://www.maxmind.com</a></div></label>`}
        <label class="check"><input type="checkbox" checked=${cfg.requireConsent} onChange=${(e) => set('requireConsent', e.target.checked)} />
          <span>Wait for consent: nothing is sent or stored until your consent banner calls <code>agg.consent({ analytics: true })</code></span></label>
      </fieldset>
      <fieldset><legend>Allowed origins</legend>
        <${TagInput} values=${cfg.allowedOrigins} onChange=${(v) => set('allowedOrigins', v)} placeholder="https://shop.example.com or https://*.example.com" mono />
        <div class="hint">Empty = accept events from any origin. The site key is public, so set this in production.</div>
      </fieldset>
      <${Alert} error=${err} ok=${msg} />
      <button class="primary" onClick=${save}>Save settings</button>
    </div>

    <${PresetCard} site=${site} reloadSites=${reloadSites} />

    <div class="card">
      <h2>Danger zone</h2>
      <p class="muted">Deleting a site removes its events, aggregates, formulas and exports.</p>
      <button class="danger" onClick=${() => setDel(true)}>Delete site</button>
    </div>
    ${del && html`<${ConfirmTyped} title="Delete site" word=${site.slug} action="Delete site"
      text="This permanently deletes all data of this site." onClose=${() => setDel(false)}
      onConfirm=${async () => { await api('DELETE', `/sites/${site.id}`); await reloadSites(); go('/'); }} />`}`;
}

render(html`<${App} />`, document.getElementById('app'));
