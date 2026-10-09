import { html, useState, useEffect, useRef, useCallback } from './vendor/preact-htm.js';

export { html, useState, useEffect, useRef, useCallback };

// ---- API

export class ApiError extends Error {
  constructor(status, body) {
    super((body && body.error) || `HTTP ${status}`);
    this.status = status;
    this.warnings = (body && body.warnings) || [];
  }
}

export async function api(method, path, body) {
  const res = await fetch('/api' + path, {
    method,
    headers: body !== undefined ? { 'Content-Type': 'application/json' } : {},
    body: body !== undefined ? JSON.stringify(body) : undefined,
    credentials: 'same-origin',
  });
  let data = null;
  try { data = await res.json(); } catch (e) { /* empty body */ }
  if (res.status === 401 && path !== '/login') {
    window.dispatchEvent(new Event('agg:logout'));
  }
  if (!res.ok) throw new ApiError(res.status, data);
  return data;
}

// useLoad runs an async loader and re-runs it when deps change. reload() refreshes manually.
export function useLoad(fn, deps, intervalMs) {
  const [state, setState] = useState({ data: null, error: null, loading: true });
  const [n, setN] = useState(0);
  useEffect(() => {
    let alive = true;
    const run = () => fn().then(
      (data) => alive && setState({ data, error: null, loading: false }),
      (error) => alive && setState((s) => ({ data: s.data, error, loading: false })),
    );
    run();
    const t = intervalMs ? setInterval(run, intervalMs) : null;
    return () => { alive = false; if (t) clearInterval(t); };
  }, [...deps, n]);
  return { ...state, reload: () => setN((x) => x + 1) };
}

// ---- routing (hash based)

export function useRoute() {
  const [hash, setHash] = useState(location.hash.slice(1) || '/');
  useEffect(() => {
    const on = () => { setHash(location.hash.slice(1) || '/'); window.scrollTo(0, 0); };
    window.addEventListener('hashchange', on);
    return () => window.removeEventListener('hashchange', on);
  }, []);
  const [path, qs] = hash.split('?');
  return { parts: path.split('/').filter(Boolean), query: new URLSearchParams(qs || '') };
}

export const go = (path) => { location.hash = path; };

// ---- formatting

const nf = new Intl.NumberFormat('en-US', { maximumFractionDigits: 2 });
const cf = new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 });

export function num(v) {
  if (v === null || v === undefined) return '–';
  if (typeof v !== 'number') return String(v);
  return nf.format(v);
}

export function compact(v) {
  if (v === null || v === undefined) return '–';
  if (typeof v !== 'number') return String(v);
  return Math.abs(v) >= 10000 ? cf.format(v) : nf.format(v);
}

export function unitValue(v, unit) {
  if (v === null || v === undefined) return '–';
  if (unit === 'percent') return nf.format(v) + '%';
  if (unit === 'duration') return duration(v);
  if (unit && unit.startsWith('currency:')) {
    try { return new Intl.NumberFormat('en-US', { style: 'currency', currency: unit.slice(9) }).format(v); } catch (e) { /* bad code */ }
  }
  return compact(v);
}

export function duration(s) {
  if (s < 60) return Math.round(s) + 's';
  if (s < 3600) return Math.round(s / 60) + ' min';
  if (s < 86400) return (s / 3600).toFixed(1).replace(/\.0$/, '') + ' h';
  return (s / 86400).toFixed(1).replace(/\.0$/, '') + ' d';
}

export function ago(unixSeconds) {
  if (!unixSeconds) return 'never';
  const d = Date.now() / 1000 - unixSeconds;
  if (d < 5) return 'just now';
  return duration(Math.max(d, 1)) + ' ago';
}

export function dateTime(unixSeconds) {
  return new Date(unixSeconds * 1000).toLocaleString();
}

export const OPS = {
  count: 'Count',
  sum: 'Sum',
  count_distinct: 'Count distinct',
  last_value: 'Last value',
  last_timestamp: 'Last time',
  avg: 'Average',
  min: 'Minimum',
  max: 'Maximum',
  p50: 'Median (p50)',
  p95: 'Percentile p95',
  p99: 'Percentile p99',
};

// Operations that summarize a numeric Value per window.
export const STAT_OPS = ['avg', 'min', 'max', 'p50', 'p95', 'p99'];
export const WINDOWED_OPS = ['count', 'sum', 'count_distinct', ...STAT_OPS];

export function describe(a) {
  let s = OPS[a.op] || a.op;
  if (a.op === 'sum' || a.op === 'last_value' || STAT_OPS.includes(a.op)) s += ` of ${a.value}`;
  if (a.op === 'count_distinct') s += a.value ? ` of ${a.value}` : ' visitors';
  s += ` · ${a.events.join(', ')}`;
  if (a.groupBy) s += ` · by ${a.groupBy.dimension}`;
  if (a.rankBy) s += ` → ${a.rankBy.dimension}`;
  return s;
}

export function lastValue(a, v) {
  if (v === null || v === undefined) return '–';
  if (a.op === 'last_timestamp') return ago(v);
  return typeof v === 'object' ? JSON.stringify(v) : num(v);
}

export function slugify(s) {
  let out = String(s || '').trim().replace(/([a-z0-9])([A-Z])/g, '$1_$2').toLowerCase()
    .replace(/[^a-z0-9]+/g, '_').replace(/^_+|_+$/g, '').slice(0, 60);
  if (out && !/^[a-z]/.test(out)) out = 'a_' + out;
  return out;
}

// ---- components

export function Alert({ error, warnings, ok }) {
  return html`
    ${error && html`<div class="alert error">${error.message || String(error)}</div>`}
    ${(warnings || []).map((w) => html`<div class="alert warn">${w}</div>`)}
    ${ok && html`<div class="alert ok">${ok}</div>`}`;
}

export function copyText(text) {
  if (navigator.clipboard) return navigator.clipboard.writeText(text);
  const t = document.createElement('textarea');
  t.value = text;
  document.body.appendChild(t);
  t.select();
  document.execCommand('copy');
  t.remove();
  return Promise.resolve();
}

export function CodeBox({ text, lang }) {
  const [done, setDone] = useState(false);
  return html`
    <div class="codebox">
      <button class="small copy" onClick=${() => copyText(text).then(() => { setDone(true); setTimeout(() => setDone(false), 1500); })}>
        ${done ? 'Copied' : 'Copy'}
      </button>
      <pre data-lang=${lang}>${text}</pre>
    </div>`;
}

export function Modal({ title, children, onClose }) {
  useEffect(() => {
    const k = (e) => e.key === 'Escape' && onClose();
    window.addEventListener('keydown', k);
    return () => window.removeEventListener('keydown', k);
  }, []);
  return html`
    <div class="modal-bg" onClick=${(e) => e.target === e.currentTarget && onClose()}>
      <div class="modal" role="dialog" aria-label=${title}>
        <h2>${title}</h2>
        ${children}
      </div>
    </div>`;
}

// ConfirmTyped asks the user to type a word before a destructive action.
export function ConfirmTyped({ title, word, text, action, onConfirm, onClose }) {
  const [v, setV] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);
  return html`
    <${Modal} title=${title} onClose=${onClose}>
      <p>${text}</p>
      <label class="field"><span>Type <code>${word}</code> to confirm</span>
        <input value=${v} onInput=${(e) => setV(e.target.value)} autofocus /></label>
      <${Alert} error=${err} />
      <div class="actions">
        <button onClick=${onClose}>Cancel</button>
        <button class="danger solid" disabled=${v !== word || busy}
          onClick=${async () => { setBusy(true); try { await onConfirm(); onClose(); } catch (e) { setErr(e); setBusy(false); } }}>
          ${busy ? 'Working…' : action}</button>
      </div>
    <//>`;
}

export function Seg({ options, value, onChange }) {
  return html`<div class="seg">${options.map((o) => html`
    <button class=${o === value ? 'on' : ''} onClick=${() => onChange(o)}>${o}</button>`)}</div>`;
}

// TagInput edits a list of strings. Enter or comma adds a value.
export function TagInput({ values, onChange, placeholder, suggestions, normalize, mono }) {
  const [v, setV] = useState('');
  const id = useRef('dl' + Math.random().toString(36).slice(2)).current;
  const add = () => {
    const x = (normalize ? normalize(v) : v.trim());
    if (x && !values.includes(x)) onChange([...values, x]);
    setV('');
  };
  return html`
    <div>
      <div>${values.map((x) => html`<span class="chip ${mono ? 'mono' : ''}">${x}
        <button title="Remove" onClick=${() => onChange(values.filter((y) => y !== x))}>×</button></span>`)}</div>
      <input value=${v} placeholder=${placeholder} list=${suggestions ? id : undefined}
        onInput=${(e) => setV(e.target.value)}
        onKeyDown=${(e) => { if (e.key === 'Enter' || e.key === ',') { e.preventDefault(); add(); } }}
        onBlur=${add} />
      ${suggestions && html`<datalist id=${id}>${suggestions.filter((s) => !values.includes(s)).map((s) => html`<option value=${s} />`)}</datalist>`}
    </div>`;
}
