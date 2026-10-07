import {
  html, useState, useEffect, useRef, api, useLoad, go, Alert, ConfirmTyped, Modal, Seg, TagInput,
  num, compact, describe, lastValue, ago, dateTime, slugify, OPS,
} from './lib.js';
import { BarChart, TopList } from './charts.js';

// ---- list

export function AggregatesPage({ site, base }) {
  const list = useLoad(() => api('GET', `/sites/${site.id}/aggregates`), [site.id], 15000);
  return html`
    <div class="page-head">
      <div><h1>Aggregates</h1><div class="sub">Counters, sums, distinct counts and last values over sliding windows, computed as events arrive.</div></div>
      <a class="btn primary" href=${`#${base}/aggregates/new`}>New aggregate</a>
    </div>
    <${Alert} error=${list.error} />
    ${list.data && html`
      <div class="card table-wrap" style="padding:4px 6px">
        ${!list.data.length ? html`<div class="empty">No aggregates yet.</div>` : html`
        <table>
          <thead><tr><th>Aggregate</th><th>Visibility</th><th class="num">Matched 24h</th><th class="num">Value 24h</th></tr></thead>
          <tbody>${list.data.map((a) => html`
            <tr class="clickable" onClick=${() => go(`${base}/aggregates/${a.id}`)}>
              <td><b>${a.title}</b> <span class="faint mono small">${a.name}</span>
                ${a.paused && html` <span class="badge">paused</span>`}
                ${a.error && html` <span class="badge bad" title=${a.error}>error</span>`}
                <div class="muted small">${describe(a)}</div></td>
              <td>${a.visibility === 'private' ? html`<span class="badge">private</span>` : html`<span class="badge accent">${a.visibility.replace('_', ' ')}</span>`}</td>
              <td class="num">${a.matched24h === 0 && !a.paused ? html`<span class="badge warn" title="No event matched in 24 hours: check event names and Where">0</span>` : num(a.matched24h)}</td>
              <td class="num">${a.op === 'last_value' || a.op === 'last_timestamp' ? lastValue(a, a.value24h) : num(a.value24h)}</td>
            </tr>`)}
          </tbody>
        </table>`}
      </div>`}`;
}

// ---- editor

const TEMPLATES = {
  blank: { label: 'Blank', def: { events: [], op: 'count' } },
  top_pages: { label: 'Top pages', def: {
    name: 'top_pages', title: 'Top pages', events: ['page_view'], op: 'count', groupBy: { dimension: 'page', expr: 'meta.path' } } },
  browsers: { label: 'Visitors per browser', def: {
    name: 'browsers', title: 'Browsers', events: ['page_view'], op: 'count_distinct', groupBy: { dimension: 'browser', expr: 'meta.browser' } } },
  feature_usage: { label: 'Feature usage', def: {
    name: 'feature_usage', title: 'Feature usage', events: ['feature_used'], op: 'count', groupBy: { dimension: 'feature', expr: 'props.feature' } } },
  feature_users: { label: 'Users per feature', def: {
    name: 'feature_users', title: 'Users per feature', events: ['feature_used'], op: 'count_distinct', groupBy: { dimension: 'feature', expr: 'props.feature' } } },
  active_users: { label: 'Active users', def: {
    name: 'active_users', title: 'Active users', events: ['page_view', 'feature_used'], op: 'count_distinct' } },
  signups: { label: 'Sign-ups per plan', def: {
    name: 'signups', title: 'Sign-ups', events: ['sign_up'], op: 'count', groupBy: { dimension: 'plan', expr: 'props.plan' } } },
  purchases: { label: 'Purchases', def: { name: 'purchases', title: 'Purchases', events: ['purchase'], op: 'count' } },
  revenue: { label: 'Revenue', def: { name: 'revenue', title: 'Revenue', events: ['purchase'], op: 'sum', value: 'props.value' } },
  product_purchases: { label: 'Purchases per product', def: {
    name: 'product_purchases', title: 'Purchases per product', events: ['purchase'], op: 'sum', explode: 'props.items',
    value: 'item.quantity ?? 1', groupBy: { dimension: 'product', expr: 'item.id', label: 'item.name' } } },
  bestsellers: { label: 'Bestseller per category', def: {
    name: 'bestsellers', title: 'Bestsellers per category', events: ['purchase'], op: 'sum', explode: 'props.items',
    value: 'item.quantity ?? 1', groupBy: { dimension: 'category', expr: 'item.category' },
    rankBy: { dimension: 'product', expr: 'item.id', label: 'item.name' } } },
  last_purchase: { label: 'Last purchase time', def: { name: 'last_purchase', title: 'Last purchase', events: ['purchase'], op: 'last_timestamp' } },
  viewers: { label: 'Visitors viewing a product', def: {
    name: 'product_viewers', title: 'Product viewers', events: ['product_view'], op: 'count_distinct',
    groupBy: { dimension: 'product', expr: 'props.id', label: 'props.name' } } },
};

const EMPTY = { name: '', title: '', events: [], where: '', explode: '', op: 'count', value: '', groupBy: null, rankBy: null,
  visibility: 'private', pinned: true, paused: false };

function toBody(d) {
  const g = (x) => (x && (x.dimension || x.expr) ? x : null);
  return { ...d, groupBy: g(d.groupBy), rankBy: d.groupBy ? g(d.rankBy) : null };
}

function Grouping({ value, onChange, dimensions, hint, placeholder }) {
  const v = value || { dimension: '', expr: '', label: '' };
  const set = (k, x) => onChange({ ...v, [k]: x });
  return html`
    <div class="inline">
      <label class="field"><span>Dimension</span>
        <input class="mono" value=${v.dimension} list="dims" placeholder=${placeholder[0]} onInput=${(e) => set('dimension', slugify(e.target.value))} />
        <div class="hint">${hint}</div></label>
      <label class="field"><span>Value</span>
        <input class="mono" value=${v.expr} placeholder=${placeholder[1]} onInput=${(e) => set('expr', e.target.value)} /></label>
    </div>
    <label class="field"><span>Label <span class="faint">(optional)</span></span>
      <input class="mono" value=${v.label || ''} placeholder=${placeholder[2]} onInput=${(e) => set('label', e.target.value)} />
      <div class="hint">A readable name shown in top lists, e.g. the product name for a product id.</div></label>
    <datalist id="dims">${dimensions.map((d) => html`<option value=${d} />`)}</datalist>`;
}

export function AggregateEditor({ site, meta, base, id, query }) {
  const isNew = !id;
  const [def, setDef] = useState(null);
  const [err, setErr] = useState(null);
  const [warns, setWarns] = useState([]);
  const [saving, setSaving] = useState(false);
  const [askRebuild, setAskRebuild] = useState(null);
  const names = useLoad(() => api('GET', `/sites/${site.id}/event-names`), [site.id]);
  const all = useLoad(() => api('GET', `/sites/${site.id}/aggregates`), [site.id]);

  useEffect(() => {
    if (isNew) {
      const ev = query.get('event');
      setDef({ ...EMPTY, events: ev ? [ev] : [] });
    } else {
      api('GET', `/sites/${site.id}/aggregates/${id}`).then((a) => setDef({ ...EMPTY, ...a }), setErr);
    }
  }, [id]);
  if (!def) return html`<${Alert} error=${err} />`;

  const set = (k, v) => setDef({ ...def, [k]: v });
  const dims = [...new Set((all.data || []).flatMap((a) => [a.groupBy && a.groupBy.dimension, a.rankBy && a.rankBy.dimension]).filter(Boolean))];
  const eventSuggestions = [...new Set([...(names.data || []).map((n) => n.name), 'page_view'])];
  const needsValue = def.op === 'sum' || def.op === 'last_value';
  const canRank = def.groupBy && (def.op === 'count' || def.op === 'sum');

  const save = async () => {
    setErr(null); setWarns([]); setSaving(true);
    try {
      const body = toBody(def);
      if (isNew) {
        const r = await api('POST', `/sites/${site.id}/aggregates`, body);
        go(`${base}/aggregates/${r.aggregate.id}`);
      } else {
        const r = await api('PUT', `/sites/${site.id}/aggregates/${id}`, body);
        setWarns(r.warnings || []);
        if (r.logicChanged) setAskRebuild(r.aggregate);
        else go(`${base}/aggregates/${id}`);
      }
    } catch (e) { setErr(e); setWarns(e.warnings || []); }
    setSaving(false);
  };

  return html`
    <div class="page-head">
      <div><h1>${isNew ? 'New aggregate' : def.title}</h1>
        <div class="sub">${isNew ? 'Pick a template or start blank. The tester on the right runs your definition on real recent events.' : html`<span class="mono">${def.name}</span>`}</div></div>
      <div class="row">
        <a class="btn" href=${isNew ? `#${base}/aggregates` : `#${base}/aggregates/${id}`}>Cancel</a>
        <button class="primary" onClick=${save} disabled=${saving}>${saving ? 'Saving…' : 'Save'}</button>
      </div>
    </div>
    <div class="grid editor">
      <div>
        ${isNew && html`
          <div class="card"><label class="field" style="margin:0"><span>Template</span>
            <select onChange=${(e) => setDef({ ...EMPTY, ...JSON.parse(JSON.stringify(TEMPLATES[e.target.value].def)) })}>
              ${Object.entries(TEMPLATES).map(([k, t]) => html`<option value=${k}>${t.label}</option>`)}
            </select></label></div>`}
        <div class="card">
          <div class="inline">
            <label class="field"><span>Title</span>
              <input value=${def.title} placeholder="Purchases per product"
                onInput=${(e) => setDef({ ...def, title: e.target.value, name: isNew && (!def.name || def.name === slugify(def.title)) ? slugify(e.target.value) : def.name })} /></label>
            <label class="field"><span>Name</span>
              <input class="mono" value=${def.name} disabled=${!isNew} onInput=${(e) => set('name', slugify(e.target.value))} />
              <div class="hint">Variables: <code>${def.name || 'name'}_24h</code>, <code>${def.name || 'name'}_prev_24h</code>…</div></label>
          </div>

          <fieldset><legend>When</legend>
            <div class="field"><span class="field-label">Events</span>
              <${TagInput} values=${def.events} onChange=${(v) => set('events', v)} placeholder="Event name, e.g. purchase" suggestions=${eventSuggestions} normalize=${slugify} mono /></div>
            <label class="field"><span>Where <span class="faint">(optional)</span></span>
              <input class="mono" value=${def.where} placeholder='props.value > 100 && props.currency == "EUR"' onInput=${(e) => set('where', e.target.value)} /></label>
            <label class="field"><span>Explode <span class="faint">(optional)</span></span>
              <input class="mono" value=${def.explode} placeholder="props.items" onInput=${(e) => set('explode', e.target.value)} />
              <div class="hint">Treat each element of an array as its own event, available as <code>item</code> (e.g. <code>item.id</code>).</div></label>
          </fieldset>

          <fieldset><legend>Calculate</legend>
            <div class="inline">
              <label class="field"><span>Operation</span>
                <select value=${def.op} onChange=${(e) => setDef({ ...def, op: e.target.value, rankBy: ['count', 'sum'].includes(e.target.value) ? def.rankBy : null })}>
                  ${Object.entries(OPS).map(([k, l]) => html`<option value=${k}>${l}</option>`)}
                </select></label>
              ${(needsValue || def.op === 'count_distinct') && html`
                <label class="field"><span>${def.op === 'count_distinct' ? 'Distinct value' : 'Value'}</span>
                  <input class="mono" value=${def.value} placeholder=${def.op === 'count_distinct' ? 'visitor (default)' : def.explode ? 'item.price * item.quantity' : 'props.value'}
                    onInput=${(e) => set('value', e.target.value)} /></label>`}
            </div>
            <div class="hint">${{
              count: 'Number of matching events (or exploded items).',
              sum: 'Sum of a numeric value. Numeric strings like "12.50" are accepted.',
              count_distinct: 'Exact number of distinct values; by default distinct visitors (needs the visitor id).',
              last_value: 'The most recent value, e.g. the last search term.',
              last_timestamp: 'When the last matching event happened.',
            }[def.op]} ${['count', 'sum', 'count_distinct'].includes(def.op) ? 'Windows: 5m, 1h, 6h, 24h, 7d, 30d and the previous period of each.' : ''}</div>
          </fieldset>

          <fieldset><legend>Group by <span class="faint">(optional)</span></legend>
            <${Grouping} value=${def.groupBy} onChange=${(v) => set('groupBy', v)} dimensions=${dims}
              hint="Name used in the API (?product=73) and Prometheus labels." placeholder=${['product', def.explode ? 'item.id' : 'props.id', def.explode ? 'item.name' : 'props.name']} />
            ${canRank && html`<details open=${!!def.rankBy}><summary>Rank within each group (e.g. bestseller per category)</summary>
              <${Grouping} value=${def.rankBy} onChange=${(v) => set('rankBy', v)} dimensions=${dims}
                hint="Top values of this dimension are ranked inside each group." placeholder=${['product', 'item.id', 'item.name']} /></details>`}
          </fieldset>

          <fieldset><legend>Options</legend>
            <label class="field"><span>Values API visibility</span>
              <select value=${def.visibility} onChange=${(e) => set('visibility', e.target.value)}>
                <option value="private">Private: admin and export tokens only</option>
                <option value="public">Public: anyone with the site key can read it from /v1/values</option>
                <option value="public_bucketed">Public, rounded: 0–9 exact, then 10+, 100+, 1000+</option>
              </select></label>
            <label class="check"><input type="checkbox" checked=${def.pinned} onChange=${(e) => set('pinned', e.target.checked)} /><span>Show on Insights</span></label>
            <label class="check"><input type="checkbox" checked=${def.paused} onChange=${(e) => set('paused', e.target.checked)} /><span>Paused (new events are ignored)</span></label>
          </fieldset>
          <${Alert} error=${err} warnings=${warns} />
          <${ExpressionHelp} />
        </div>
      </div>
      <div class="sticky"><${Tester} site=${site} def=${toBody(def)} /></div>
    </div>
    ${askRebuild && html`<${RebuildModal} site=${site} agg=${askRebuild} onClose=${() => go(`${base}/aggregates/${id}`)} meta=${meta} afterSave />`}`;
}

function ExpressionHelp() {
  return html`
    <details><summary>Expression reference</summary>
      <div class="small muted">
        <p><code>event</code> event name · <code>props</code> what you sent · <code>meta</code> added by agg: <code>meta.path</code>, <code>meta.referrer</code>, <code>meta.language</code>, <code>meta.browser</code>, <code>meta.os</code>, <code>meta.device</code> (desktop, mobile, tablet), <code>meta.bot</code>, <code>meta.ip</code> (if enabled) ·
          <code>item</code> current element when exploding · <code>visitor</code> visitor id · <code>ts</code> unix time.</p>
        <p>Operators: <code>== != &lt; &gt; &lt;= &gt;=</code>, <code>&& || !</code>, <code>in</code> (<code>props.currency in ["EUR", "PLN"]</code>), <code>contains</code>, <code>startsWith</code>, <code>matches</code> (regex), <code>+ - * /</code>, <code>??</code> (default: <code>item.quantity ?? 1</code>).</p>
        <p>Missing fields are <code>nil</code>; an event whose Where fails or errors is not counted.</p>
      </div>
    </details>`;
}

// Tester runs the current definition against recent stored events or pasted JSON, without saving.
function Tester({ site, def }) {
  const [mode, setMode] = useState('recent');
  const [paste, setPaste] = useState('[\n  { "name": "purchase", "props": { "order_id": "A-1", "value": 120, "items": [{ "id": "73", "name": "Shoe", "category": "shoes", "price": 60, "quantity": 2 }] },\n    "meta": { "path": "/thank-you", "browser": "Chrome", "device": "mobile" } }\n]');
  const [onlyMatching, setOnlyMatching] = useState(false);
  const [res, setRes] = useState(null);
  const [err, setErr] = useState(null);
  const [open, setOpen] = useState({});
  const key = JSON.stringify(def) + mode + (mode === 'paste' ? paste : '');
  const timer = useRef(null);
  const run = async () => {
    setErr(null);
    let body = { def };
    if (mode === 'paste') {
      try { const v = JSON.parse(paste); body.events = Array.isArray(v) ? v : [v]; } catch (e) { setErr(new Error('Invalid JSON: ' + e.message)); return; }
    }
    if (!def.events.length) { setRes(null); return; }
    try { setRes(await api('POST', `/sites/${site.id}/dry-run`, body)); } catch (e) { setErr(e); setRes(null); }
  };
  useEffect(() => { clearTimeout(timer.current); timer.current = setTimeout(run, 400); }, [key]);
  const results = res ? res.results.filter((r) => !onlyMatching || r.matched) : [];
  const matched = res ? res.results.filter((r) => r.matched).length : 0;
  return html`
    <div class="card">
      <div class="row" style="margin-bottom:10px"><h2 style="margin:0">Tester</h2><span class="spacer"></span>
        <${Seg} options=${['recent', 'paste']} value=${mode} onChange=${setMode} /></div>
      ${mode === 'paste' && html`<textarea value=${paste} onInput=${(e) => setPaste(e.target.value)} style="margin-bottom:10px"></textarea>`}
      <div class="row small" style="margin-bottom:10px">
        <span class="muted">${res ? `${matched} of ${res.results.length} ${mode === 'recent' ? 'recent events' : 'events'} match` : def.events.length ? '' : 'Choose an event to test'}</span>
        <span class="spacer"></span>
        <label class="check" style="margin:0"><input type="checkbox" checked=${onlyMatching} onChange=${(e) => setOnlyMatching(e.target.checked)} /> Matching only</label>
        <button class="small" onClick=${run}>Run</button>
      </div>
      <${Alert} error=${err} warnings=${res && res.warnings} />
      ${res && mode === 'recent' && !res.results.length && html`<div class="empty small">No stored events named ${def.events.join(', ')} yet. Paste one to test.</div>`}
      <div class="tester-results">${results.map((r, i) => html`
        <div class="event ${r.matched ? 'matched' : ''}">
          <div class="event-head" onClick=${() => setOpen({ ...open, [i]: !open[i] })}>
            <b>${r.event.name}</b>
            ${r.matched ? html`<span class="badge accent">${r.contributions.length} contribution${r.contributions.length === 1 ? '' : 's'}</span>` : html`<span class="badge">no match</span>`}
            ${r.error && html`<span class="badge bad" title=${r.error}>error</span>`}
            <span class="spacer"></span>
            ${r.event.receivedAt && html`<span class="faint small">${ago(r.event.receivedAt / 1000)}</span>`}
          </div>
          ${r.matched && html`<div style="padding:4px 10px 6px">${r.contributions.map((c) => html`<div class="contrib">${contribText(def, c)}</div>`)}</div>`}
          ${r.error && html`<div class="alert error" style="margin:4px 10px">${r.error}</div>`}
          ${open[i] && html`<div class="event-body"><pre>${JSON.stringify(r.event.props || {}, null, 2)}</pre></div>`}
        </div>`)}</div>
    </div>`;
}

function contribText(def, c) {
  const parts = [];
  if (def.groupBy) parts.push(`${def.groupBy.dimension}=${c.group || '∅'}${c.groupLabel ? ` (${c.groupLabel})` : ''}`);
  if (def.rankBy) parts.push(`${def.rankBy.dimension}=${c.rank || '∅'}${c.rankLabel ? ` (${c.rankLabel})` : ''}`);
  if (def.op === 'count') parts.push('+1');
  if (def.op === 'sum') parts.push(`+${num(c.value || 0)}`);
  if (def.op === 'count_distinct') parts.push(`distinct ${c.distinct}`);
  if (def.op === 'last_value') parts.push(`= ${c.raw}`);
  if (def.op === 'last_timestamp') parts.push('time = now');
  return parts.join(' · ');
}

function RebuildModal({ site, agg, meta, onClose, afterSave }) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);
  return html`
    <${Modal} title=${afterSave ? 'Recalculate with the new definition?' : 'Rebuild from raw events'} onClose=${onClose}>
      ${afterSave && html`<p>Saved. The new definition applies to new events. Existing values were computed with the old one.</p>`}
      <p class="muted">Rebuild clears this aggregate and replays the stored raw events (last ${meta.rawRetentionDays} days) through the new definition. Older data cannot be recalculated.</p>
      <${Alert} error=${err} />
      <div class="actions">
        <button onClick=${onClose}>${afterSave ? 'Keep existing data' : 'Cancel'}</button>
        <button class="primary" disabled=${busy} onClick=${async () => {
          setBusy(true);
          try { await api('POST', `/sites/${site.id}/aggregates/${agg.id}/rebuild`); onClose(); } catch (e) { setErr(e); setBusy(false); }
        }}>${busy ? 'Rebuilding…' : 'Rebuild from raw events'}</button>
      </div>
    <//>`;
}

// ---- detail

export function AggregateDetail({ site, meta, base, id }) {
  const agg = useLoad(() => api('GET', `/sites/${site.id}/aggregates/${id}`), [id]);
  const [dims, setDims] = useState({});
  const [range, setRange] = useState('24h');
  const [topWin, setTopWin] = useState('24h');
  const [topBy, setTopBy] = useState('');
  const [modal, setModal] = useState(null);
  const a = agg.data;
  const dimQS = new URLSearchParams(Object.entries(dims).filter(([, v]) => v)).toString();
  const values = useLoad(() => api('GET', `/sites/${site.id}/aggregates/${id}/values?${dimQS}`), [id, dimQS], 15000);
  const windowed = a && ['count', 'sum', 'count_distinct'].includes(a.op);
  const series = useLoad(() => windowed ? api('GET', `/sites/${site.id}/aggregates/${id}/series?range=${range}&${dimQS}`) : Promise.resolve(null), [id, range, dimQS, windowed], 30000);
  const top = useLoad(() => a && a.groupBy
    ? api('GET', `/sites/${site.id}/aggregates/${id}/top?window=${topWin}&limit=20${topBy ? '&by=' + topBy : ''}&${dimQS}`)
    : Promise.resolve(null), [id, topWin, topBy, dimQS, a && a.id]);
  if (!a) return html`<${Alert} error=${agg.error} />`;
  const v = values.data;
  const dimNames = [a.groupBy && a.groupBy.dimension, a.rankBy && a.rankBy.dimension].filter(Boolean);
  const fmt = a.op === 'last_timestamp' ? ago : num;
  // Without a group-by there is no Top card, so History sits next to Values instead of below.
  const history = html`
    <div class="card">
      <div class="row" style="margin-bottom:10px"><h2 style="margin:0">History</h2><span class="spacer"></span>
        <${Seg} options=${meta.ranges} value=${range} onChange=${setRange} /></div>
      ${series.data && html`<${BarChart} points=${series.data.points} step=${series.data.step} format=${compact}
        width=${a.groupBy ? 800 : 560} height=${a.groupBy ? 220 : 280} />`}
    </div>`;

  return html`
    <div class="page-head">
      <div><h1>${a.title}</h1>
        <div class="sub"><span class="mono">${a.name}</span> · ${describe(a)}
          ${a.paused && html` · <span class="badge">paused</span>`}</div></div>
      <div class="row">
        <a class="btn" href=${`#${base}/aggregates/${id}/edit`}>Edit</a>
        <button onClick=${() => setModal('rebuild')}>Rebuild</button>
        <button onClick=${() => setModal('reset')}>Reset data</button>
        <button class="danger" onClick=${() => setModal('delete')}>Delete</button>
      </div>
    </div>
    ${a.dataResetAt > 0 && html`<p class="faint small">Data since ${dateTime(a.dataResetAt / 1000)} (last reset or rebuild).</p>`}

    ${dimNames.length > 0 && html`
      <div class="card"><div class="row">
        <span class="muted">Narrow to</span>
        ${dimNames.map((d) => html`<label class="row" style="gap:6px"><span class="mono small">${d} =</span>
          <input style="width:180px" class="mono" placeholder="all" value=${dims[d] || ''} onChange=${(e) => setDims({ ...dims, [d]: e.target.value.trim() })} /></label>`)}
        ${dimQS && html`<button class="small" onClick=${() => setDims({})}>Clear</button>`}
      </div></div>`}

    <div class=${a.groupBy ? 'grid two' : 'grid detail'} style="margin-top:16px">
      <div class="card">
        <h2>Values</h2>
        ${v && windowed && html`
          <table>
            <thead><tr><th>Window</th><th class="num">Value</th><th class="num">Previous</th><th class="num">Change</th></tr></thead>
            <tbody>${v.windows.map((w) => html`
              <tr><td class="mono">${w.window}</td><td class="num"><b>${num(w.value)}</b></td><td class="num muted">${num(w.prev)}</td>
                <td class="num muted">${w.change === null ? '–' : (w.change >= 0 ? '+' : '') + w.change.toFixed(0) + '%'}</td></tr>`)}
              ${v.total !== undefined && html`<tr><td>All time</td><td class="num"><b>${num(v.total)}</b></td><td></td><td></td></tr>`}
            </tbody>
          </table>`}
        ${v && !windowed && html`<div class="tile"><div class="value">${lastValue(a, v.last)}</div>
          ${v.lastAt && html`<div class="delta">${dateTime(v.lastAt)}</div>`}</div>`}
        <details style="margin-top:12px"><summary>Variables (for formulas and the values API)</summary>
          <div>${a.variables.map((n) => html`<span class="chip mono">${n}</span>`)}</div></details>
      </div>
      ${a.groupBy ? html`
        <div class="card">
          <div class="row" style="margin-bottom:10px"><h2 style="margin:0">Top</h2><span class="spacer"></span>
            ${a.rankBy && html`<select style="width:auto" value=${topBy || a.rankBy.dimension} onChange=${(e) => setTopBy(e.target.value)}>
              <option value=${a.rankBy.dimension}>${a.rankBy.dimension}</option><option value=${a.groupBy.dimension}>${a.groupBy.dimension}</option></select>`}
            ${windowed && html`<${Seg} options=${meta.windows} value=${topWin} onChange=${setTopWin} />`}
          </div>
          <${Alert} error=${top.error} />
          ${top.data && html`<${TopList} items=${top.data.items} format=${fmt} />`}
          ${a.rankBy && !dims[a.groupBy.dimension] && (topBy || a.rankBy.dimension) === a.rankBy.dimension && html`
            <p class="hint">Across all ${a.groupBy.dimension} values. Narrow to one ${a.groupBy.dimension} above to rank inside it.</p>`}
        </div>` : windowed ? history : html`<div></div>`}
    </div>

    ${windowed && a.groupBy && html`<div style="margin-top:16px">${history}</div>`}

    ${modal === 'reset' && html`<${ConfirmTyped} title="Reset data" word=${a.name} action="Reset data" onClose=${() => setModal(null)}
      text="Deletes all collected values of this aggregate. New events keep being counted. Raw events are kept, so you can rebuild later."
      onConfirm=${async () => { await api('POST', `/sites/${site.id}/aggregates/${id}/reset`); values.reload(); series.reload(); top.reload(); agg.reload(); }} />`}
    ${modal === 'delete' && html`<${ConfirmTyped} title="Delete aggregate" word=${a.name} action="Delete" onClose=${() => setModal(null)}
      text="Deletes the aggregate and its data. Formulas and exports that use it will stop showing it."
      onConfirm=${async () => { await api('DELETE', `/sites/${site.id}/aggregates/${id}`); go(`${base}/aggregates`); }} />`}
    ${modal === 'rebuild' && html`<${RebuildModal} site=${site} agg=${a} meta=${meta} onClose=${() => { setModal(null); values.reload(); series.reload(); top.reload(); }} />`}`;
}
