import { html, useState, useEffect, api, useLoad, Alert, Seg, num, ago } from './lib.js';

// ---- events: a live stream and an audit log of stored raw events

export function EventsPage({ site, base, meta, query }) {
  const [mode, setMode] = useState(query.get('visitor') || query.get('mode') === 'audit' ? 'Audit' : 'Live');
  return html`
    <div class="page-head">
      <div><h1>Events</h1><div class="sub">${mode === 'Live'
        ? 'What your site sends, after the privacy filter. Refreshes every 5 s.'
        : `Every stored event, newest first, with filters. Raw events are kept ${meta.rawRetentionDays} days.`}</div></div>
      <${Seg} options=${['Live', 'Audit']} value=${mode} onChange=${setMode} />
    </div>
    ${mode === 'Live' ? html`<${LiveEvents} site=${site} base=${base} />`
      : html`<${AuditLog} site=${site} base=${base} visitor=${query.get('visitor') || ''} />`}`;
}

function LiveEvents({ site, base }) {
  const [name, setName] = useState('');
  const [open, setOpen] = useState({});
  const counts = useLoad(() => api('GET', `/sites/${site.id}/event-names`), [site.id], 10000);
  const recent = useLoad(() => api('GET', `/sites/${site.id}/events?limit=100${name ? '&name=' + encodeURIComponent(name) : ''}`), [site.id, name], 5000);
  return html`
    <div class="grid editor">
      <div class="card">
        <div class="row" style="margin-bottom:10px"><h2 style="margin:0">Recent events</h2><span class="spacer"></span>
          ${name && html`<button class="small" onClick=${() => setName('')}>Show all</button>`}</div>
        <${Alert} error=${recent.error} />
        ${recent.data && !recent.data.length && html`<div class="empty">No events yet. Install the snippet from <a href=${`#${base}/settings`}>Settings</a> or see the <a href=${`#${base}/guide`}>tracking guide</a>.</div>`}
        ${(recent.data || []).map((e) => html`
          <div class="event">
            <div class="event-head" onClick=${() => setOpen({ ...open, [e.rawId]: !open[e.rawId] })}>
              <b>${e.name}</b>
              <span class="faint small mono">${e.meta && e.meta.path ? e.meta.path : ''}</span>
              <span class="spacer"></span>
              <span class="faint small">${ago(e.receivedAt / 1000)}</span>
            </div>
            ${open[e.rawId] && html`<div class="event-body">
              <${EventJSON} e=${e} />
              <div style="margin-top:8px"><a class="btn small" href=${`#${base}/aggregates/new?event=${encodeURIComponent(e.name)}`}>Create aggregate from ${e.name}</a></div>
            </div>`}
          </div>`)}
      </div>
      <div class="card sticky">
        <h2>Last 24 hours</h2>
        <table>
          <thead><tr><th>Event</th><th class="num">Count</th></tr></thead>
          <tbody>${(counts.data || []).map((c) => html`
            <tr class="clickable" onClick=${() => setName(c.name)}><td>${c.name === name ? html`<b>${c.name}</b>` : c.name}</td><td class="num">${num(c.count)}</td></tr>`)}
          </tbody>
        </table>
        ${counts.data && !counts.data.length && html`<div class="empty small">Nothing yet</div>`}
      </div>
    </div>`;
}

const EventJSON = ({ e }) => html`<pre>${JSON.stringify({ name: e.name, id: e.id, visitorId: e.visitorId, ts: e.ts, props: e.props, meta: e.meta }, null, 2)}</pre>`;

const LIMITS = [50, 100, 500, 1000];

// stamp formats milliseconds as local "YYYY-MM-DD HH:MM:SS".
function stamp(ms) {
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function AuditLog({ site, base, visitor }) {
  const names = useLoad(() => api('GET', `/sites/${site.id}/event-names`), [site.id]);
  const [draft, setDraft] = useState({ name: '', visitor, limit: 100 });
  const [f, setF] = useState(draft);
  const [rows, setRows] = useState(null);
  const [more, setMore] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);
  const [open, setOpen] = useState({});

  const load = async (before) => {
    setBusy(true); setErr(null);
    try {
      const qs = new URLSearchParams({ limit: f.limit });
      if (f.name) qs.set('name', f.name);
      if (f.visitor) qs.set('visitor', f.visitor);
      if (before) qs.set('before', before);
      const evs = await api('GET', `/sites/${site.id}/events?${qs}`);
      setRows((cur) => (before ? [...cur, ...evs] : evs));
      setMore(evs.length === f.limit);
    } catch (e) { setErr(e); }
    setBusy(false);
  };
  useEffect(() => { setOpen({}); load(); }, [site.id, f]);

  const apply = (next) => { setDraft(next); setF(next); };
  const download = () => {
    const blob = new Blob([JSON.stringify(rows, null, 2)], { type: 'application/json' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = `agg-${site.slug}-events-${new Date().toISOString().slice(0, 19).replace(/:/g, '')}.json`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  };

  return html`
    <form class="card audit-filters" onSubmit=${(e) => { e.preventDefault(); setF({ ...draft }); }}>
      <label><span class="field-label">Event</span>
        <select value=${draft.name} onChange=${(e) => setDraft({ ...draft, name: e.target.value })}>
          <option value="">All events</option>
          ${(names.data || []).map((n) => html`<option value=${n.name}>${n.name}</option>`)}
          ${draft.name && !(names.data || []).some((n) => n.name === draft.name) && html`<option value=${draft.name}>${draft.name}</option>`}
        </select></label>
      <label><span class="field-label">Visitor id</span>
        <input class="mono" value=${draft.visitor} placeholder="any visitor" onInput=${(e) => setDraft({ ...draft, visitor: e.target.value.trim() })} /></label>
      <label><span class="field-label">Show</span>
        <select value=${draft.limit} onChange=${(e) => setDraft({ ...draft, limit: +e.target.value })}>
          ${LIMITS.map((n) => html`<option value=${n}>last ${n}</option>`)}</select></label>
      <div class="audit-actions">
        <button class="primary" disabled=${busy}>Apply</button>
        ${(f.name || f.visitor) && html`<button type="button" onClick=${() => apply({ name: '', visitor: '', limit: f.limit })}>Clear</button>`}
      </div>
    </form>

    <div class="card" style="padding:4px 6px">
      <${Alert} error=${err} />
      <div class="row audit-bar">
        <span class="muted small">${rows ? `${num(rows.length)} event${rows.length === 1 ? '' : 's'}` : 'Loading…'}
          ${f.name && html` · <b>${f.name}</b>`}${f.visitor && html` · visitor <code>${f.visitor}</code>`}</span>
        <span class="spacer"></span>
        <button class="small" type="button" disabled=${busy} onClick=${() => load()}>Refresh</button>
        <button class="small" type="button" disabled=${!rows || !rows.length} onClick=${download}>Download JSON</button>
      </div>
      ${rows && !rows.length && html`<div class="empty">No stored events match. <a href=${`#${base}/guide`}>How to send events</a></div>`}
      ${rows && rows.length > 0 && html`
        <div class="table-wrap"><table class="audit">
          <thead><tr><th>Received</th><th>Event</th><th>Visitor</th><th>Path</th><th>Client</th><th>Event id</th></tr></thead>
          <tbody>${rows.map((e) => html`
            <tr class="clickable ${open[e.rawId] ? 'open' : ''}" onClick=${() => setOpen({ ...open, [e.rawId]: !open[e.rawId] })}>
              <td class="mono nowrap" title=${ago(e.receivedAt / 1000)}>${stamp(e.receivedAt)}</td>
              <td><b>${e.name}</b></td>
              <td class="mono">${e.visitorId
                ? html`<button class="link mono" title="Show only this visitor" onClick=${(ev) => { ev.stopPropagation(); apply({ ...f, visitor: e.visitorId }); }}>${e.visitorId.slice(0, 12)}${e.visitorId.length > 12 ? '…' : ''}</button>`
                : html`<span class="faint">–</span>`}</td>
              <td class="mono ellipsis">${(e.meta && e.meta.path) || ''}</td>
              <td class="small muted nowrap">${e.meta ? [e.meta.browser, e.meta.os, e.meta.device].filter(Boolean).join(' · ') : ''}
                ${e.meta && e.meta.bot && html` <span class="badge warn">bot</span>`}</td>
              <td class="mono small">${e.id || html`<span class="faint">–</span>`}</td>
            </tr>
            ${open[e.rawId] && html`<tr class="detail-row"><td colspan="6">
              <div class="event-body">
                <div class="small muted" style="margin-bottom:6px">Received ${new Date(e.receivedAt).toLocaleString()}
                  ${e.ts ? ` · client time ${new Date(e.ts).toLocaleString()}` : ''} · raw id ${e.rawId}</div>
                <${EventJSON} e=${e} />
              </div></td></tr>`}`)}
          </tbody>
        </table></div>`}
      ${more && html`<div class="center" style="padding:10px">
        <button disabled=${busy} onClick=${() => load(rows[rows.length - 1].rawId)}>${busy ? 'Loading…' : `Load ${f.limit} older`}</button></div>`}
    </div>`;
}
