import {
  html, useState, useEffect, useRef, api, useLoad, Alert, CodeBox, ConfirmTyped, Modal, TagInput,
  num, unitValue, ago, slugify,
} from './lib.js';

// ---- formulas

const EMPTY_FORMULA = { name: '', title: '', expr: '', unit: 'number', visibility: 'private', pinned: true };

export function FormulasPage({ site }) {
  const list = useLoad(() => api('GET', `/sites/${site.id}/formulas`), [site.id], 30000);
  const vars = useLoad(() => api('GET', `/sites/${site.id}/variables`), [site.id]);
  const [edit, setEdit] = useState(null);
  const [del, setDel] = useState(null);
  return html`
    <div class="page-head">
      <div><h1>Formulas</h1><div class="sub">Derived metrics computed when read, e.g. average order value or conversion rate.</div></div>
      <button class="primary" onClick=${() => setEdit({ ...EMPTY_FORMULA })}>New formula</button>
    </div>
    <${Alert} error=${list.error} />
    ${edit && html`<${FormulaEditor} site=${site} formula=${edit} vars=${(vars.data || []).filter((v) => v.kind === 'aggregate')}
      onDone=${() => { setEdit(null); list.reload(); }} />`}
    ${list.data && html`
      <div class="card table-wrap" style="padding:4px 6px">
        ${!list.data.length ? html`<div class="empty">No formulas yet. Example: <code>revenue_24h / purchases_24h</code></div>` : html`
        <table>
          <thead><tr><th>Formula</th><th>Expression</th><th class="num">Value</th><th></th></tr></thead>
          <tbody>${list.data.map((f) => html`
            <tr><td><b>${f.title}</b> <span class="faint mono small">${f.name}</span>
                ${f.visibility === 'public' && html` <span class="badge accent">public</span>`}</td>
              <td class="mono small">${f.expr}${f.error && html`<div class="badge bad">${f.error}</div>`}</td>
              <td class="num"><b>${unitValue(f.value, f.unit)}</b></td>
              <td class="num"><button class="small" onClick=${() => setEdit(f)}>Edit</button>
                <button class="small danger" onClick=${() => setDel(f)}>Delete</button></td></tr>`)}
          </tbody>
        </table>`}
      </div>`}
    ${del && html`<${ConfirmTyped} title="Delete formula" word=${del.name} action="Delete" text="Exports and API calls using it will return null."
      onClose=${() => setDel(null)} onConfirm=${async () => { await api('DELETE', `/sites/${site.id}/formulas/${del.id}`); list.reload(); }} />`}`;
}

function FormulaEditor({ site, formula, vars, onDone }) {
  const [f, setF] = useState(formula);
  const [preview, setPreview] = useState(null);
  const [err, setErr] = useState(null);
  const input = useRef(null);
  const timer = useRef(null);
  const set = (k, v) => setF({ ...f, [k]: v });
  useEffect(() => {
    clearTimeout(timer.current);
    if (!f.expr.trim()) { setPreview(null); return; }
    timer.current = setTimeout(() => api('POST', `/sites/${site.id}/formulas/preview`, { expr: f.expr }).then(setPreview, setErr), 300);
  }, [f.expr]);
  const insert = (name) => {
    const el = input.current;
    const pos = el ? el.selectionStart : f.expr.length;
    set('expr', f.expr.slice(0, pos) + name + f.expr.slice(pos));
  };
  const save = async () => {
    setErr(null);
    try {
      if (f.id) await api('PUT', `/sites/${site.id}/formulas/${f.id}`, f);
      else await api('POST', `/sites/${site.id}/formulas`, f);
      onDone();
    } catch (e) { setErr(e); }
  };
  const currency = f.unit.startsWith('currency:');
  return html`
    <${Modal} title=${f.id ? 'Edit formula' : 'New formula'} onClose=${onDone}>
      <div class="inline">
        <label class="field"><span>Title</span><input value=${f.title} placeholder="Average order value"
          onInput=${(e) => setF({ ...f, title: e.target.value, name: f.id ? f.name : slugify(e.target.value) })} /></label>
        <label class="field"><span>Name</span><input class="mono" value=${f.name} disabled=${!!f.id} onInput=${(e) => set('name', slugify(e.target.value))} /></label>
      </div>
      <label class="field"><span>Expression</span>
        <input class="mono" ref=${input} value=${f.expr} placeholder="revenue_24h / purchases_24h" onInput=${(e) => set('expr', e.target.value)} />
        <div class="hint"><code>+ - * /</code>, parentheses, <code>round() abs() min() max()</code>, <code>coalesce(x, 0)</code>. Division by zero and missing values give no value (never Infinity).</div></label>
      <div style="max-height:110px;overflow:auto;margin-bottom:12px">${vars.map((v) => html`<button class="chip" onClick=${() => insert(v.name)}>${v.name}</button>`)}</div>
      <div class="inline">
        <label class="field"><span>Unit</span>
          <select value=${currency ? 'currency' : f.unit} onChange=${(e) => set('unit', e.target.value === 'currency' ? 'currency:EUR' : e.target.value)}>
            <option value="number">Number</option><option value="percent">Percent</option><option value="currency">Currency</option><option value="duration">Duration (seconds)</option>
          </select></label>
        ${currency ? html`<label class="field"><span>Currency code</span><input class="mono" value=${f.unit.slice(9)} maxlength="3"
          onInput=${(e) => set('unit', 'currency:' + e.target.value.toUpperCase())} /></label>` : html`
        <label class="field"><span>Values API</span><select value=${f.visibility} onChange=${(e) => set('visibility', e.target.value)}>
          <option value="private">Private</option><option value="public">Public</option></select></label>`}
      </div>
      <label class="check"><input type="checkbox" checked=${f.pinned} onChange=${(e) => set('pinned', e.target.checked)} /><span>Show on Insights</span></label>
      ${preview && html`<div class="codebox small" style="margin-top:8px">
        ${preview.error ? html`<span class="badge bad">${preview.error}</span>` : html`
          <div>Value now: <b>${unitValue(preview.value, f.unit)}</b></div>
          <div class="muted mono">${Object.entries(preview.variables || {}).map(([k, v]) => `${k} = ${v === null ? 'null' : num(v)}`).join(' · ')}</div>`}
      </div>`}
      <${Alert} error=${err} />
      <div class="actions"><button onClick=${onDone}>Cancel</button><button class="primary" onClick=${save}>Save</button></div>
    <//>`;
}

// ---- exports

const EMPTY_EXPORT = { name: '', format: 'prometheus', cacheSeconds: 15,
  scope: { aggregates: [], formulas: [], windows: ['1h', '24h'], partitions: 'none', topK: 10, allow: [] } };

export function ExportsPage({ site, meta }) {
  const list = useLoad(() => api('GET', `/sites/${site.id}/exports`), [site.id], 30000);
  const [edit, setEdit] = useState(null);
  const [created, setCreated] = useState(null);
  const [del, setDel] = useState(null);
  const [preview, setPreview] = useState(null);
  const rotate = async (e) => {
    if (!confirm(`Generate a new token for "${e.name}"? The current token stops working immediately.`)) return;
    setCreated(await api('POST', `/sites/${site.id}/exports/${e.id}/rotate`));
  };
  return html`
    <div class="page-head">
      <div><h1>Exports</h1><div class="sub">Read-only, token-protected endpoints for Prometheus, Grafana and scripts.</div></div>
      <button class="primary" onClick=${() => setEdit(JSON.parse(JSON.stringify(EMPTY_EXPORT)))}>New export</button>
    </div>
    <${Alert} error=${list.error} />
    ${list.data && html`
      <div class="card table-wrap" style="padding:4px 6px">
        ${!list.data.length ? html`<div class="empty">No exports yet. Create one to scrape values with Prometheus or read them as JSON.</div>` : html`
        <table>
          <thead><tr><th>Export</th><th>Scope</th><th class="num">Last scrape</th><th></th></tr></thead>
          <tbody>${list.data.map((e) => html`
            <tr><td><b>${e.name}</b> <span class="badge">${e.format}</span><div class="faint mono small">/export/${e.id}/metrics</div></td>
              <td class="small muted">${[...e.scope.aggregates, ...e.scope.formulas].join(', ')}<br />
                windows ${e.scope.windows.join(', ')} · ${e.scope.partitions === 'topk' ? `top ${e.scope.topK} per dimension` : e.scope.partitions === 'allowlist' ? `${e.scope.allow.length} allowed values` : 'no dimensions'}</td>
              <td class="num small">${e.lastScrapedAt ? ago(e.lastScrapedAt / 1000) : 'never'}<div class="faint">${num(e.scrapeCount)} scrapes</div></td>
              <td class="num">
                <button class="small" onClick=${() => setPreview(e)}>Preview</button>
                <button class="small" onClick=${() => setEdit(JSON.parse(JSON.stringify(e)))}>Edit</button>
                <button class="small" onClick=${() => rotate(e)}>New token</button>
                <button class="small danger" onClick=${() => setDel(e)}>Delete</button></td></tr>`)}
          </tbody>
        </table>`}
      </div>`}
    <div class="card"><h2>How it works</h2>
      <p class="muted">Prometheus scrapes the current values; it keeps history and computes rates itself. Window values are gauges
        (<code>agg_value{aggregate, window}</code>), all-time counts are counters (<code>agg_events_total</code>, use <code>increase()</code>), plus <code>agg_last_timestamp_seconds</code>, <code>agg_last_value</code> and <code>agg_formula_value</code>. Per-dimension series (e.g. per product) go to <code>agg_dimension_value</code> and <code>agg_dimension_events_total</code>, only with a top-K or an allowlist, so the number of series stays bounded and <code>sum()</code> never double counts.</p>
    </div>
    ${edit && html`<${ExportEditor} site=${site} meta=${meta} value=${edit} onClose=${() => setEdit(null)}
      onSaved=${(r) => { setEdit(null); list.reload(); if (r && r.token) setCreated(r); }} />`}
    ${created && html`<${TokenModal} meta=${meta} created=${created} onClose=${() => { setCreated(null); list.reload(); }} />`}
    ${preview && html`<${PreviewModal} site=${site} e=${preview} onClose=${() => setPreview(null)} />`}
    ${del && html`<${ConfirmTyped} title="Delete export" word=${del.name} action="Delete" text="The URL and token stop working immediately."
      onClose=${() => setDel(null)} onConfirm=${async () => { await api('DELETE', `/sites/${site.id}/exports/${del.id}`); list.reload(); }} />`}`;
}

function toggle(list, v, on) { return on ? [...list, v] : list.filter((x) => x !== v); }

function ExportEditor({ site, meta, value, onClose, onSaved }) {
  const [e, setE] = useState(value);
  const [err, setErr] = useState(null);
  const [pv, setPv] = useState(null);
  const aggs = useLoad(() => api('GET', `/sites/${site.id}/aggregates`), [site.id]);
  const fs = useLoad(() => api('GET', `/sites/${site.id}/formulas`), [site.id]);
  const timer = useRef(null);
  const sc = e.scope;
  const setScope = (k, v) => setE({ ...e, scope: { ...sc, [k]: v } });
  useEffect(() => {
    clearTimeout(timer.current);
    timer.current = setTimeout(() => api('POST', `/sites/${site.id}/exports/preview`, e).then(setPv, setErr), 300);
  }, [JSON.stringify(e)]);
  const save = async () => {
    setErr(null);
    try {
      if (e.id) { await api('PUT', `/sites/${site.id}/exports/${e.id}`, e); onSaved(null); }
      else onSaved(await api('POST', `/sites/${site.id}/exports`, e));
    } catch (x) { setErr(x); }
  };
  const grouped = (aggs.data || []).some((a) => a.groupBy && sc.aggregates.includes(a.name));
  return html`
    <${Modal} title=${e.id ? 'Edit export' : 'New export'} onClose=${onClose}>
      <div class="inline">
        <label class="field"><span>Name</span><input value=${e.name} placeholder="prometheus" onInput=${(x) => setE({ ...e, name: x.target.value })} /></label>
        <label class="field"><span>Format</span><select value=${e.format} onChange=${(x) => setE({ ...e, format: x.target.value })}>
          <option value="prometheus">Prometheus text format</option><option value="json">JSON (Grafana Infinity, scripts)</option></select></label>
      </div>
      <div class="field-label">Aggregates</div>
      <div class="checks">${(aggs.data || []).map((a) => html`<label class="check"><input type="checkbox" checked=${sc.aggregates.includes(a.name)}
        onChange=${(x) => setScope('aggregates', toggle(sc.aggregates, a.name, x.target.checked))} /><span class="mono small">${a.name}</span></label>`)}</div>
      ${(fs.data || []).length > 0 && html`<div class="field-label" style="margin-top:8px">Formulas</div>
        <div class="checks">${fs.data.map((f) => html`<label class="check"><input type="checkbox" checked=${sc.formulas.includes(f.name)}
          onChange=${(x) => setScope('formulas', toggle(sc.formulas, f.name, x.target.checked))} /><span class="mono small">${f.name}</span></label>`)}</div>`}
      <div class="field-label" style="margin-top:8px">Windows</div>
      <div class="row">${meta.windows.map((w) => html`<label class="check"><input type="checkbox" checked=${sc.windows.includes(w)}
        onChange=${(x) => setScope('windows', toggle(sc.windows, w, x.target.checked))} /><span class="mono small">${w}</span></label>`)}</div>
      ${grouped && html`
        <label class="field" style="margin-top:8px"><span>Per-dimension series</span>
          <select value=${sc.partitions} onChange=${(x) => setScope('partitions', x.target.value)}>
            <option value="none">None: totals only</option><option value="topk">Top K values per window</option><option value="allowlist">Only listed values</option></select></label>
        ${sc.partitions === 'topk' && html`<label class="field"><span>K</span><input type="number" min="1" max="1000" value=${sc.topK}
          onInput=${(x) => setScope('topK', parseInt(x.target.value, 10) || 0)} /></label>`}
        ${sc.partitions === 'allowlist' && html`<div class="field"><span class="field-label">Values</span>
          <${TagInput} values=${sc.allow} onChange=${(v) => setScope('allow', v)} placeholder="e.g. product id 73" mono /></div>`}`}
      <label class="field"><span>Cache (seconds)</span><input type="number" min="0" max="3600" value=${e.cacheSeconds}
        onInput=${(x) => setE({ ...e, cacheSeconds: parseInt(x.target.value, 10) || 0 })} /></label>
      ${pv && html`<div class="small" style="margin-bottom:6px">${pv.error ? html`<span class="badge warn">${pv.error}</span>`
        : html`<b>${num(pv.series)}</b> series per scrape`}</div>
        ${pv.body && html`<div class="codebox"><pre style="max-height:180px">${pv.body}</pre></div>`}`}
      <${Alert} error=${err} />
      <div class="actions"><button onClick=${onClose}>Cancel</button><button class="primary" onClick=${save}>${e.id ? 'Save' : 'Create'}</button></div>
    <//>`;
}

function TokenModal({ meta, created, onClose }) {
  const { export: e, token } = created;
  const url = `${meta.baseUrl}/export/${e.id}/metrics`;
  const u = new URL(meta.baseUrl);
  const scrape = `scrape_configs:
  - job_name: agg_${e.name.replace(/[^a-zA-Z0-9_]/g, '_')}
    scrape_interval: 60s
    scheme: ${u.protocol.replace(':', '')}
    metrics_path: /export/${e.id}/metrics
    authorization:
      type: Bearer
      credentials: ${token}
    static_configs:
      - targets: ['${u.host}']`;
  return html`
    <${Modal} title="Export token" onClose=${onClose}>
      <div class="alert warn">Copy the token now. It is stored hashed and cannot be shown again.</div>
      <div class="field"><span class="field-label">Token</span><${CodeBox} text=${token} /></div>
      <div class="field"><span class="field-label">Test it</span><${CodeBox} text=${`curl -H "Authorization: Bearer ${token}" ${url}`} /></div>
      ${e.format === 'prometheus'
        ? html`<div class="field"><span class="field-label">prometheus.yml</span><${CodeBox} text=${scrape} /></div>`
        : html`<p class="muted small">Grafana Infinity: source URL <code>${url}</code>, type JSON, header <code>Authorization: Bearer …</code>, rows/root <code>metrics</code>.</p>`}
      <div class="actions"><button class="primary" onClick=${onClose}>Done</button></div>
    <//>`;
}

function PreviewModal({ site, e, onClose }) {
  const pv = useLoad(() => api('GET', `/sites/${site.id}/exports/${e.id}/preview`), [e.id]);
  return html`
    <${Modal} title=${`Preview: ${e.name}`} onClose=${onClose}>
      <${Alert} error=${pv.error} />
      ${pv.data && html`<p class="small"><b>${num(pv.data.series)}</b> series</p><${CodeBox} text=${pv.data.body} />`}
      <div class="actions"><button onClick=${onClose}>Close</button></div>
    <//>`;
}

// ---- values API explorer

export function ApiPage({ site, meta }) {
  const vars = useLoad(() => api('GET', `/sites/${site.id}/variables`), [site.id]);
  const [sel, setSel] = useState([]);
  const [dims, setDims] = useState({});
  const [asPublic, setAsPublic] = useState(true);
  const [filter, setFilter] = useState('');
  const [res, setRes] = useState(null);
  const [err, setErr] = useState(null);
  const all = vars.data || [];
  const chosen = all.filter((v) => sel.includes(v.name));
  const dimNames = [...new Set(chosen.flatMap((v) => v.dimensions))];
  const dimQS = dimNames.filter((d) => dims[d]).map((d) => `&${d}=${encodeURIComponent(dims[d])}`).join('');
  const publicUrl = `${meta.baseUrl}/v1/values?site=${site.publicKey}&v=${sel.join(',')}${dimQS}`;
  useEffect(() => {
    if (!sel.length) { setRes(null); return; }
    api('GET', `/sites/${site.id}/values?v=${sel.join(',')}${dimQS}${asPublic ? '&public=1' : ''}`).then((r) => { setRes(r); setErr(null); }, setErr);
  }, [sel.join(','), dimQS, asPublic]);
  const shown = all.filter((v) => !filter || v.name.includes(filter));
  return html`
    <div class="page-head">
      <div><h1>Values API</h1><div class="sub">Read values as JSON from your site or another app. Public variables need only the site key; responses are cached for 30 s.</div></div>
    </div>
    <div class="grid editor">
      <div class="card">
        <h2>Variables</h2>
        <input placeholder="Filter, e.g. _24h" value=${filter} onInput=${(e) => setFilter(e.target.value)} style="margin-bottom:10px" />
        <div style="max-height:420px;overflow:auto">
          ${shown.map((v) => html`<label class="check"><input type="checkbox" checked=${sel.includes(v.name)}
            onChange=${(e) => setSel(toggle(sel, v.name, e.target.checked))} />
            <span><span class="mono small">${v.name}</span> ${v.visibility === 'private' ? html`<span class="badge">private</span>` : html`<span class="badge accent">${v.visibility.replace('_', ' ')}</span>`}
              ${v.kind === 'formula' && html` <span class="badge">formula</span>`}</span></label>`)}
        </div>
        <p class="hint">Make an aggregate public in its settings to read it without a token.</p>
      </div>
      <div>
        <div class="card">
          ${dimNames.map((d) => html`<label class="field"><span class="mono">${d}</span>
            <input class="mono" value=${dims[d] || ''} placeholder="all" onInput=${(e) => setDims({ ...dims, [d]: e.target.value })} /></label>`)}
          <label class="check"><input type="checkbox" checked=${asPublic} onChange=${(e) => setAsPublic(e.target.checked)} />
            <span>Show what the public endpoint returns (private values are null)</span></label>
          <div class="field"><span class="field-label">URL</span><${CodeBox} text=${publicUrl} /></div>
          <div class="field"><span class="field-label">Response</span>
            <${Alert} error=${err} />
            <${CodeBox} text=${res ? JSON.stringify(res, null, 2) : 'Select variables'} /></div>
          <div class="field"><span class="field-label">JavaScript</span><${CodeBox} text=${`const res = await fetch('${publicUrl}');
const { values } = await res.json();
// values.${sel[0] || 'purchases_24h'} is a number, or null when there is no value: show nothing then.`} /></div>
          <p class="hint">Top lists: <code>${meta.baseUrl}/v1/top?site=${site.publicKey}&aggregate=NAME&window=24h&limit=10</code></p>
        </div>
      </div>
    </div>`;
}
