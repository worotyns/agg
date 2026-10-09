/*! agg browser SDK | Elastic License 2.0 */
(function (w, d) {
  'use strict';
  if (w.agg && w.agg._loaded) return;

  var MAX_QUEUE = 500, MAX_BATCH = 50, MAX_BYTES = 60000, MAX_PROPS = 8192, DEBOUNCE = 1000, MAX_DELAY = 60000;

  // "Sign Up" / "signUp" -> "sign_up" (same rules as the server)
  function norm(name) {
    return String(name == null ? '' : name).trim()
      .replace(/([a-z0-9])([A-Z])/g, '$1_$2').toLowerCase()
      .replace(/[^a-z0-9]+/g, '_').replace(/^_+|_+$/g, '').slice(0, 64);
  }

  function store(fn) {
    try { return fn(w.localStorage); } catch (e) { return null; } // storage blocked: keep going in memory
  }

  var cfg = null, endpoint = '', site = '', queue = [], inflight = 0, timer = null, delay = 0;
  var consent = null, vid = null, lastPath = null, referrer = null, pending = [];
  // Page time: visible milliseconds of the current page, sent as page_leave when it is hidden or navigated away from.
  var timePath = null, shownAt = null, shownMs = 0;

  function qkey() { return 'agg_q_' + site; }
  function persist() {
    store(function (s) { s.setItem(qkey(), JSON.stringify(queue.slice(inflight))); });
  }

  function visitorId() {
    if (!cfg.visitorId) return undefined;
    if (vid) return vid;
    vid = store(function (s) { return s.getItem('agg_vid'); });
    if (!vid) {
      vid = w.crypto && w.crypto.randomUUID ? w.crypto.randomUUID() : Date.now().toString(36) + Math.random().toString(36).slice(2);
      store(function (s) { s.setItem('agg_vid', vid); });
    }
    return vid;
  }

  function allowed() { return cfg && (!cfg.requireConsent || consent === true); }

  // Page context sent with every event; the server adds browser, os and device from the User-Agent.
  function meta(withReferrer, path) {
    var m = { path: path || w.location.pathname };
    if (w.navigator && w.navigator.language) m.language = w.navigator.language;
    if (withReferrer && referrer) m.referrer = referrer;
    return m;
  }

  function track(name, props, id, withReferrer, path) {
    name = norm(name);
    if (!name) return;
    if (!cfg) { pending.push([name, props, id, withReferrer, path]); return; }
    if (cfg.requireConsent && consent === false) return;
    var ev = { name: name, ts: Date.now(), meta: meta(withReferrer, path) };
    if (id != null && id !== '') ev.id = String(id);
    if (props && typeof props === 'object') {
      var json = JSON.stringify(props);
      if (json.length > MAX_PROPS) { if (w.console) console.warn('[agg] props over 8 KB, event dropped:', name); return; }
      if (json.length > 2) ev.props = JSON.parse(json);
    }
    queue.push(ev);
    if (queue.length > MAX_QUEUE) queue.splice(inflight, queue.length - MAX_QUEUE); // drop the oldest waiting events
    persist();
    schedule(DEBOUNCE);
  }

  function schedule(ms) {
    if (timer || !allowed()) return;
    timer = setTimeout(function () { timer = null; flush(); }, ms);
  }

  // takeBatch returns the next events to send (≤ 50 events, ≤ 60 KB), with the visitor id added.
  function takeBatch(from) {
    var id = visitorId(), batch = [], size = 0;
    for (var i = from; i < queue.length && batch.length < MAX_BATCH; i++) {
      if (id) queue[i].visitorId = id;
      var s = JSON.stringify(queue[i]).length + 1;
      if (batch.length && size + s > MAX_BYTES) break;
      batch.push(queue[i]);
      size += s;
    }
    return batch;
  }

  function body(batch) { return JSON.stringify({ site: site, events: batch }); }

  // flush sends one batch. Network errors, 429 and 5xx are retried with exponential backoff (1 s … 60 s);
  // other 4xx responses mean the batch is invalid, so it is dropped.
  function flush() {
    if (inflight || !allowed() || !queue.length) return;
    var batch = takeBatch(0);
    inflight = batch.length;
    persist();
    var done = function (ok) {
      if (ok) { queue.splice(0, inflight); delay = 0; } else { delay = Math.min(MAX_DELAY, Math.max(1000, delay * 2)); }
      inflight = 0;
      persist();
      if (queue.length) schedule(ok ? 0 : delay);
    };
    w.fetch(endpoint + '/e', { method: 'POST', body: body(batch), keepalive: true, headers: { 'Content-Type': 'text/plain' } })
      .then(function (r) { done(r.ok || (r.status >= 400 && r.status < 500 && r.status !== 429)); })
      .catch(function () { done(false); });
  }

  // On page hide, hand the waiting events to the browser with sendBeacon (delivered after the page is gone).
  function flushOnHide() {
    if (!allowed()) return;
    while (queue.length > inflight) {
      var batch = takeBatch(inflight);
      if (!w.navigator.sendBeacon || !w.navigator.sendBeacon(endpoint + '/e', new Blob([body(batch)], { type: 'text/plain' }))) break;
      queue.splice(inflight, batch.length);
    }
    persist();
  }

  // leaveTime sends the visible time spent on the current page since the last call (one event per visible stretch).
  function leaveTime() {
    if (!cfg || !cfg.pageTime || timePath === null) return;
    var now = Date.now(), ms = shownMs + (shownAt === null ? 0 : now - shownAt);
    shownMs = 0;
    shownAt = d.visibilityState === 'hidden' ? null : now;
    if (ms > 0) track('page_leave', { ms: Math.round(ms) }, null, false, timePath);
  }

  function onHide() { leaveTime(); shownAt = null; flushOnHide(); }

  function page() {
    var path = w.location.pathname;
    if (path === lastPath) return;
    var first = lastPath === null;
    if (timePath !== null && timePath !== path) leaveTime();
    if (timePath !== null) timePath = path;
    lastPath = path;
    track('page_view', null, null, first);
  }

  function watch() {
    var h = w.history;
    if (cfg.pageViews && h && h.pushState) {
      var orig = h.pushState;
      h.pushState = function () { var r = orig.apply(h, arguments); setTimeout(page, 0); return r; };
      w.addEventListener('popstate', page);
    }
    w.addEventListener('pagehide', onHide);
    d.addEventListener('visibilitychange', function () {
      if (d.visibilityState === 'hidden') onHide();
      else if (shownAt === null) shownAt = Date.now();
    });
    w.addEventListener('online', function () { delay = 0; schedule(0); });
  }

  function start(config) {
    cfg = config;
    if (d.referrer) {
      try {
        var host = new URL(d.referrer).hostname;
        if (host && host !== w.location.hostname) referrer = host;
      } catch (e) { /* invalid referrer */ }
    }
    var saved = store(function (s) { return JSON.parse(s.getItem(qkey()) || '[]'); });
    if (saved && saved.length) queue = saved.concat(queue); // events an earlier page could not deliver
    if (cfg.pageTime) {
      timePath = w.location.pathname;
      shownAt = d.visibilityState === 'hidden' ? null : Date.now();
    }
    watch();
    if (cfg.pageViews) page();
    var p = pending;
    pending = [];
    for (var i = 0; i < p.length; i++) track(p[i][0], p[i][1], p[i][2], p[i][3], p[i][4]);
    if (queue.length) schedule(DEBOUNCE);
  }

  var DEFAULTS = { visitorId: true, pageViews: true, pageTime: true, requireConsent: false };

  var api = {
    _loaded: true,
    version: '2',
    // init({ endpoint, site, config? }); config skips the /v1/config request.
    init: function (opts) {
      opts = opts || {};
      if (cfg || !opts.site) return;
      site = opts.site;
      endpoint = String(opts.endpoint || '').replace(/\/+$/, '');
      if (opts.config) return start(opts.config);
      w.fetch(endpoint + '/v1/config?site=' + encodeURIComponent(site))
        .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
        .then(start, function (e) { if (w.console) console.warn('[agg] config:', e.message); start(DEFAULTS); });
    },
    track: function (name, props, id) { track(name, props, id, false); },
    page: page,
    consent: function (c) {
      consent = !!(c && c.analytics);
      if (consent) schedule(0); else { queue = queue.slice(0, inflight); persist(); }
    },
    flush: flush,
    _norm: norm
  };

  // Calls made before the script loaded: window.agg = { q: [['track', ...], ...] }
  var stub = w.agg && w.agg.q;
  w.agg = api;
  var script = d.currentScript;
  if (script && script.getAttribute('data-site')) {
    api.init({ site: script.getAttribute('data-site'), endpoint: new URL(script.src).origin });
  }
  if (stub && stub.length) {
    for (var i = 0; i < stub.length; i++) {
      var call = stub[i];
      if (call && typeof api[call[0]] === 'function') api[call[0]].apply(api, [].slice.call(call, 1));
    }
  }
})(window, document);
