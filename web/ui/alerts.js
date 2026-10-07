import {
  html, useState, useEffect, useRef, api, useLoad, Alert, CodeBox, ConfirmTyped, Modal, ago, num, slugify,
} from './lib.js';

const STATE_BADGE = { ok: 'good', pending: 'warn', firing: 'bad' };
const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const TZ = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';

// ---- browser push

function b64ToBytes(s) {
  const pad = '='.repeat((4 - (s.length % 4)) % 4);
  const raw = atob((s + pad).replace(/-/g, '+').replace(/_/g, '/'));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
}

export function PushPanel() {
  const info = useLoad(() => api('GET', '/push'), []);
  const [state, setState] = useState('checking');
  const [msg, setMsg] = useState(null);
  const [err, setErr] = useState(null);
  const supported = 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;

  const refresh = async () => {
    if (!supported) return setState('unsupported');
    if (!window.isSecureContext) return setState('insecure');
    if (Notification.permission === 'denied') return setState('blocked');
    const reg = await navigator.serviceWorker.getRegistration('/');
    const sub = reg && (await reg.pushManager.getSubscription());
    setState(sub ? 'on' : 'off');
  };
  useEffect(() => { refresh(); }, []);

  const enable = async () => {
    setErr(null);
    try {
      const reg = await navigator.serviceWorker.register('/sw.js', { scope: '/' });
      await navigator.serviceWorker.ready;
      if ((await Notification.requestPermission()) !== 'granted') { setState('blocked'); return; }
      const sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64ToBytes(info.data.publicKey) });
      await api('POST', '/push/subscriptions', sub.toJSON());
      setState('on'); info.reload();
    } catch (e) { setErr(e); }
  };
  const disable = async () => {
    const reg = await navigator.serviceWorker.getRegistration('/');
    const sub = reg && (await reg.pushManager.getSubscription());
    if (sub) { await api('DELETE', '/push/subscriptions', { endpoint: sub.endpoint }); await sub.unsubscribe(); }
    setState('off'); info.reload();
  };
  const test = async () => {
    setErr(null); setMsg(null);
    try {
      const r = await api('POST', '/push/test');
      const bad = r.filter((x) => x.error);
      setMsg(bad.length ? null : `Sent to ${r.length} device${r.length === 1 ? '' : 's'}.`);
      if (bad.length) setErr(new Error(bad.map((x) => x.error).join('; ')));
    } catch (e) { setErr(e); }
  };
  const devices = info.data ? info.data.subscriptions.length : 0;
  return html`
    <div class="card">
      <div class="row">
        <div><h2 style="margin:0">Browser notifications</h2>
          <div class="muted small">${{
            checking: '…',
            on: 'On for this browser.',
            off: 'Off for this browser.',
            blocked: 'Blocked in this browser’s site settings. Allow notifications for this site and reload.',
            unsupported: 'This browser does not support Web Push.',
            insecure: 'Needs HTTPS (or localhost). Open agg through your HTTPS address.',
          }[state]} ${devices} device${devices === 1 ? '' : 's'} subscribed in total.</div></div>
        <span class="spacer"></span>
        ${state === 'off' && html`<button class="primary" onClick=${enable}>Enable on this device</button>`}
        ${state === 'on' && html`<button onClick=${disable}>Disable here</button>`}
        ${devices > 0 && html`<button onClick=${test}>Send test</button>`}
      </div>
      <${Alert} error=${err} ok=${msg} />
    </div>`;
}

// ---- notifications bell

export function Bell() {
  const [open, setOpen] = useState(false);
  const n = useLoad(() => api('GET', '/notifications'), [], 30000);
  const unread = n.data ? n.data.unread : 0;
  const toggle = async () => {
    setOpen(!open);
    if (!open && unread) { await api('POST', '/notifications/read'); n.reload(); }
  };
  return html`
    <div class="bell-wrap">
      <button class="bell" title="Notifications" aria-label=${`Notifications, ${unread} unread`} onClick=${toggle}>
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <path d="M18 8a6 6 0 0 0-12 0c0 7-3 9-3 9h18s-3-2-3-9" /><path d="M13.7 21a2 2 0 0 1-3.4 0" /></svg>
        ${unread > 0 && html`<span class="dot">${unread > 9 ? '9+' : unread}</span>`}
      </button>
      ${open && html`
        <div class="bell-panel" onClick=${(e) => e.stopPropagation()}>
          <div class="row" style="margin-bottom:6px"><b>Notifications</b><span class="spacer"></span>
            <button class="link small" onClick=${() => setOpen(false)}>Close</button></div>
          ${n.data && !n.data.items.length && html`<div class="empty small">No notifications yet. Alerts send them here, to your browser and to webhooks.</div>`}
          ${n.data && n.data.items.map((x) => html`
            <a class="note ${x.read ? '' : 'unread'}" href=${x.url ? '#' + x.url.split('#')[1] : '#'} onClick=${() => setOpen(false)}>
              <div class="row"><b>${x.title}</b><span class="spacer"></span><span class="faint small">${ago(x.ts / 1000)}</span></div>
              <div class="muted small">${x.body}</div>
            </a>`)}
        </div>`}
    </div>`;
}

// ---- alerts page

const EMPTY = { name: '', title: '', condition: '', dims: {}, forMinutes: 5, cooldownMinutes: 60, notifyResolved: true, push: true,
  webhook: '', enabled: true, schedule: { days: [], fromHour: 0, toHour: 0, timezone: TZ } };

const EXAMPLES = [
  ['No orders for 3 hours', 'now - last_purchase > 3 * 3600'],
  ['Revenue down 50%', 'revenue_prev_24h > 0 && revenue_24h < 0.5 * revenue_prev_24h'],
  ['Traffic stopped', 'page_views_total > 0 && page_views_6h < 1'],
  ['Product suddenly popular', 'product_viewers_5m > 50'],
];

export function AlertsPage({ site }) {
  const list = useLoad(() => api('GET', `/sites/${site.id}/alerts`), [site.id], 30000);
  const vars = useLoad(() => api('GET', `/sites/${site.id}/variables`), [site.id]);
  const [edit, setEdit] = useState(null);
  const [del, setDel] = useState(null);
  const [hist, setHist] = useState(null);
  const [msg, setMsg] = useState(null);
  return html`
    <div class="page-head">
      <div><h1>Alerts</h1><div class="sub">Checked every minute. Notifications go to this page’s bell, to subscribed browsers and to webhooks.</div></div>
      <button class="primary" onClick=${() => setEdit(JSON.parse(JSON.stringify(EMPTY)))}>New alert</button>
    </div>
    <${PushPanel} />
    <${Alert} error=${list.error} ok=${msg} />
    ${list.data && html`
      <div class="card table-wrap" style="padding:4px 6px">
        ${!list.data.length ? html`<div class="empty">No alerts yet. Example: <code>orders_1h < 1</code> for an hour without orders.</div>` : html`
        <table>
          <thead><tr><th>Alert</th><th>State</th><th>Now</th><th></th></tr></thead>
          <tbody>${list.data.map((a) => html`
            <tr>
              <td><b>${a.title || a.name}</b> <span class="faint mono small">${a.name}</span>
                <div class="mono small muted">${a.condition}${a.forMinutes ? ` · for ${a.forMinutes} min` : ''}</div>
                <div class="small faint">${[a.push && 'browser', a.webhook && 'webhook'].filter(Boolean).join(' + ') || 'in-app only'}
                  ${a.schedule.fromHour !== a.schedule.toHour ? ` · ${a.schedule.fromHour}:00–${a.schedule.toHour}:00` : ''}
                  ${a.schedule.days.length ? ` · ${a.schedule.days.map((d) => DAYS[d]).join(', ')}` : ''}</div></td>
              <td>${!a.enabled ? html`<span class="badge">disabled</span>` : html`<span class="badge ${STATE_BADGE[a.state]}">${a.state}</span>`}
                ${a.stateSince > 0 && html`<div class="faint small">${ago(a.stateSince / 1000)}</div>`}</td>
              <td class="small">${a.lastError ? html`<span class="badge warn" title=${a.lastError}>${a.lastError}</span>` : html`<span class="mono muted">${a.lastValue || '–'}</span>`}
                ${a.lastEval > 0 && html`<div class="faint small">checked ${ago(a.lastEval / 1000)}</div>`}</td>
              <td class="num"><div class="btn-group">
                <button class="small" onClick=${() => setHist(a)}>History</button>
                <button class="small" onClick=${async () => { await api('POST', `/sites/${site.id}/alerts/${a.id}/test`); setMsg(`Test notification sent for “${a.title || a.name}”.`); }}>Test</button>
                <button class="small" onClick=${() => setEdit(JSON.parse(JSON.stringify(a)))}>Edit</button>
                <button class="small danger" onClick=${() => setDel(a)}>Delete</button></div></td>
            </tr>`)}
          </tbody>
        </table>`}
      </div>`}
    ${edit && html`<${AlertEditor} site=${site} value=${edit} vars=${vars.data || []} onClose=${() => setEdit(null)} onSaved=${() => { setEdit(null); list.reload(); }} />`}
    ${hist && html`<${History} site=${site} a=${hist} onClose=${() => setHist(null)} />`}
    ${del && html`<${ConfirmTyped} title="Delete alert" word=${del.name} action="Delete" text="The alert and its history are deleted."
      onClose=${() => setDel(null)} onConfirm=${async () => { await api('DELETE', `/sites/${site.id}/alerts/${del.id}`); list.reload(); }} />`}`;
}

function History({ site, a, onClose }) {
  const evs = useLoad(() => api('GET', `/sites/${site.id}/alerts/${a.id}/events`), [a.id]);
  return html`
    <${Modal} title=${`History: ${a.title || a.name}`} onClose=${onClose}>
      ${evs.data && !evs.data.length && html`<div class="empty small">No state changes yet.</div>`}
      ${evs.data && html`<table><tbody>${evs.data.map((e) => html`
        <tr><td class="small faint" style="white-space:nowrap">${new Date(e.ts).toLocaleString()}</td>
          <td><span class="badge ${STATE_BADGE[e.state]}">${e.state}</span></td><td class="small mono">${e.message}</td></tr>`)}</tbody></table>`}
      <div class="actions"><button onClick=${onClose}>Close</button></div>
    <//>`;
}

function AlertEditor({ site, value, vars, onClose, onSaved }) {
  const [a, setA] = useState(value);
  const [err, setErr] = useState(null);
  const [check, setCheck] = useState(null);
  const timer = useRef(null);
  const input = useRef(null);
  const set = (k, v) => setA({ ...a, [k]: v });
  const setSched = (k, v) => setA({ ...a, schedule: { ...a.schedule, [k]: v } });
  useEffect(() => {
    clearTimeout(timer.current);
    if (!a.condition.trim()) { setCheck(null); return; }
    timer.current = setTimeout(() => api('POST', `/sites/${site.id}/alerts/check`, { condition: a.condition, dims: a.dims })
      .then(setCheck, (e) => setCheck({ error: e.message })), 300);
  }, [a.condition, JSON.stringify(a.dims)]);
  const insert = (name) => {
    const pos = input.current ? input.current.selectionStart : a.condition.length;
    set('condition', a.condition.slice(0, pos) + name + a.condition.slice(pos));
  };
  const save = async () => {
    setErr(null);
    try {
      if (a.id) await api('PUT', `/sites/${site.id}/alerts/${a.id}`, a);
      else await api('POST', `/sites/${site.id}/alerts`, a);
      onSaved();
    } catch (e) { setErr(e); }
  };
  const shown = vars.filter((v) => !v.name.includes('_prev_') || a.condition.includes('prev'));
  return html`
    <${Modal} title=${a.id ? 'Edit alert' : 'New alert'} onClose=${onClose}>
      <div class="inline">
        <label class="field"><span>Title</span><input value=${a.title} placeholder="No orders for an hour"
          onInput=${(e) => setA({ ...a, title: e.target.value, name: a.id ? a.name : slugify(e.target.value) })} /></label>
        <label class="field"><span>Name</span><input class="mono" value=${a.name} disabled=${!!a.id} onInput=${(e) => set('name', slugify(e.target.value))} /></label>
      </div>
      <label class="field"><span>Condition</span>
        <input class="mono" ref=${input} value=${a.condition} placeholder="orders_1h < 1" onInput=${(e) => set('condition', e.target.value)} />
        <div class="hint">True means “alert”. Use variables, formulas and <code>now</code> (unix seconds), e.g. <code>now - last_purchase > 7200</code>.</div></label>
      ${!a.condition && html`<div style="margin:-6px 0 10px">${EXAMPLES.map(([t, c]) => html`<button class="chip" title=${c} onClick=${() => setA({ ...a, condition: c, title: a.title || t, name: a.name || slugify(t) })}>${t}</button>`)}</div>`}
      <div style="max-height:90px;overflow:auto;margin-bottom:10px">${shown.map((v) => html`<button class="chip" onClick=${() => insert(v.name)}>${v.name}</button>`)}
        <button class="chip" onClick=${() => insert('now')}>now</button></div>
      ${check && html`<div class="codebox small" style="margin-bottom:12px">
        ${check.error ? html`<span class="badge warn">${check.error}</span>` : html`
          Now: <b>${check.result ? 'would alert' : 'ok'}</b>
          <span class="muted mono"> · ${Object.entries(check.values || {}).map(([k, v]) => `${k} = ${v === null ? 'no value' : num(v)}`).join(', ')}</span>`}
      </div>`}
      <div class="inline">
        <label class="field"><span>For (minutes)</span><input type="number" min="0" value=${a.forMinutes} onInput=${(e) => set('forMinutes', parseInt(e.target.value, 10) || 0)} />
          <div class="hint">Condition must hold this long.</div></label>
        <label class="field"><span>Cooldown (minutes)</span><input type="number" min="0" value=${a.cooldownMinutes} onInput=${(e) => set('cooldownMinutes', parseInt(e.target.value, 10) || 0)} />
          <div class="hint">Minimum time between notifications.</div></label>
      </div>
      <fieldset><legend>Notify</legend>
        <label class="check"><input type="checkbox" checked=${a.push} onChange=${(e) => set('push', e.target.checked)} /><span>Browser notification on subscribed devices</span></label>
        <label class="field"><span>Webhook <span class="faint">(optional)</span></span>
          <input class="mono" value=${a.webhook} placeholder="https://hooks.slack.com/services/…" onInput=${(e) => set('webhook', e.target.value.trim())} />
          <div class="hint">JSON POST; works directly with Slack, Mattermost and Discord webhooks.</div></label>
        <label class="check"><input type="checkbox" checked=${a.notifyResolved} onChange=${(e) => set('notifyResolved', e.target.checked)} /><span>Also notify when resolved</span></label>
      </fieldset>
      <fieldset><legend>Only notify</legend>
        <div class="row" style="margin-bottom:8px">${DAYS.map((d, i) => html`<label class="check" style="margin:0"><input type="checkbox" checked=${a.schedule.days.includes(i)}
          onChange=${(e) => setSched('days', e.target.checked ? [...a.schedule.days, i] : a.schedule.days.filter((x) => x !== i))} /><span class="small">${d}</span></label>`)}</div>
        <div class="row">
          <span class="small muted">from</span><input type="number" min="0" max="24" style="width:70px" value=${a.schedule.fromHour} onInput=${(e) => setSched('fromHour', parseInt(e.target.value, 10) || 0)} />
          <span class="small muted">to</span><input type="number" min="0" max="24" style="width:70px" value=${a.schedule.toHour} onInput=${(e) => setSched('toHour', parseInt(e.target.value, 10) || 0)} />
          <input class="mono" style="width:180px" value=${a.schedule.timezone} onInput=${(e) => setSched('timezone', e.target.value)} />
        </div>
        <div class="hint">No days ticked = every day. Same from and to = all day. Outside these times alerts still change state but stay silent.</div>
      </fieldset>
      <label class="check"><input type="checkbox" checked=${a.enabled} onChange=${(e) => set('enabled', e.target.checked)} /><span>Enabled</span></label>
      <${Alert} error=${err} />
      <div class="actions"><button onClick=${onClose}>Cancel</button><button class="primary" onClick=${save}>Save</button></div>
    <//>`;
}

// ---- API & MCP

export function AccessPage({ meta }) {
  const list = useLoad(() => api('GET', '/tokens'), []);
  const [name, setName] = useState('');
  const [created, setCreated] = useState(null);
  const [err, setErr] = useState(null);
  const [del, setDel] = useState(null);
  const create = async (e) => {
    e.preventDefault(); setErr(null);
    try { setCreated(await api('POST', '/tokens', { name })); setName(''); list.reload(); } catch (x) { setErr(x); }
  };
  const mcpURL = meta.baseUrl + '/mcp';
  const tok = created ? created.token : '<API token>';
  return html`
    <div class="page-head">
      <div><h1>API & MCP</h1><div class="sub">Named tokens for scripts and AI assistants. They can use the admin API and the MCP server, not the UI login. Revoke them any time.</div></div>
    </div>
    <div class="card">
      <h2>Tokens</h2>
      <form class="row" onSubmit=${create} style="margin-bottom:12px">
        <input style="max-width:320px" value=${name} placeholder="Name, e.g. Claude on my laptop" onInput=${(e) => setName(e.target.value)} />
        <button class="primary" disabled=${!name.trim()}>Create token</button>
      </form>
      <${Alert} error=${err} />
      ${created && html`<div class="alert warn">Copy the token now; it is stored hashed and cannot be shown again.</div><${CodeBox} text=${created.token} />`}
      ${list.data && list.data.length > 0 && html`
        <table style="margin-top:12px"><thead><tr><th>Name</th><th>Token</th><th>Created</th><th>Last used</th><th></th></tr></thead>
          <tbody>${list.data.map((t) => html`<tr><td>${t.name}</td><td class="mono small">${t.prefix}…</td><td class="small">${ago(t.createdAt / 1000)}</td>
            <td class="small">${t.lastUsedAt ? ago(t.lastUsedAt / 1000) : 'never'}</td>
            <td class="num"><button class="small danger" onClick=${() => setDel(t)}>Revoke</button></td></tr>`)}</tbody></table>`}
    </div>
    <div class="card">
      <h2>Connect an AI assistant (MCP)</h2>
      <p class="muted">The MCP server lets an assistant read your numbers and plan and apply configuration: sites, presets, aggregates
        (tested on real events first), formulas, alerts and exports. It asks before deleting.</p>
      <div class="field"><span class="field-label">Claude Code</span>
        <${CodeBox} text=${`claude mcp add --transport http agg ${mcpURL} --header "Authorization: Bearer ${tok}"`} /></div>
      <div class="field"><span class="field-label">JSON config (Claude Desktop, Cursor, VS Code and other clients with HTTP MCP)</span>
        <${CodeBox} text=${JSON.stringify({ mcpServers: { agg: { type: 'http', url: mcpURL, headers: { Authorization: 'Bearer ' + tok } } } }, null, 2)} /></div>
      <p class="hint">Then ask, for example: “Plan analytics for my SaaS: which features do paying users use?” (prompt <code>plan_setup</code>),
        or “Which products sold best this week?”. Docs for assistants: <a href="/llms.txt" target="_blank">/llms.txt</a>.</p>
    </div>
    <div class="card">
      <h2>Admin API</h2>
      <p class="muted">Every UI action is an HTTP call under <code>/api</code>. Send <code>${'Authorization: Bearer <token>'}</code>.</p>
      <${CodeBox} text=${`curl -H "Authorization: Bearer ${tok}" ${meta.baseUrl}/api/sites`} />
    </div>
    ${del && html`<${ConfirmTyped} title="Revoke token" word=${del.name} action="Revoke" text="Programs using this token lose access immediately."
      onClose=${() => setDel(null)} onConfirm=${async () => { await api('DELETE', `/tokens/${del.id}`); list.reload(); }} />`}`;
}
