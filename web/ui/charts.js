import { html, useState, num } from './lib.js';

function bucketLabel(t, step, long) {
  const d = new Date(t * 1000);
  if (step >= 86400) return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  const time = d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
  if (!long) return time;
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) + ' ' + time;
}

// Sparkline: thin bars, the current (partial) bucket in the accent color, earlier ones muted.
export function Sparkline({ points }) {
  if (!points || !points.length) return null;
  const max = Math.max(1, ...points.map((p) => p.v));
  const n = points.length;
  const w = 100 / n;
  const gap = n > 60 ? 0.12 : 0.7;
  return html`
    <svg class="spark" viewBox="0 0 100 40" preserveAspectRatio="none" aria-hidden="true">
      ${points.map((p, i) => {
        const h = p.v ? Math.max(2.5, (p.v / max) * 36) : 1;
        const last = i === n - 1;
        return html`<rect x=${i * w + gap / 2} y=${40 - h} width=${Math.max(w - gap, 0.25)} height=${h}
          fill=${last ? 'var(--accent)' : 'var(--bar)'} opacity=${last ? 1 : p.v ? 0.75 : 0.3} rx="0.8" />`;
      })}
    </svg>`;
}

function niceMax(v) {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

// BarChart: one series over time with a y-axis, recessive grid and a hover tooltip per bar.
// width is the viewBox width: pass roughly the rendered width so axis text stays ~11px.
export function BarChart({ points, step, height = 220, width = 800, format = num }) {
  const [hover, setHover] = useState(null);
  if (!points || !points.length) return html`<div class="empty">No data</div>`;
  const W = width, H = height, padL = 44, padB = 22, padT = 8;
  const max = niceMax(Math.max(...points.map((p) => p.v)));
  const n = points.length;
  const bw = (W - padL) / n;
  const gap = Math.min(2, bw * 0.25);
  const y = (v) => padT + (H - padT - padB) * (1 - v / max);
  const ticks = [0, max / 2, max];
  const labelEvery = Math.ceil(n / Math.max(3, Math.floor(W / 100)));
  return html`
    <div class="chart" onMouseLeave=${() => setHover(null)}>
      <svg viewBox="0 0 ${W} ${H}" role="img" aria-label="Bar chart">
        ${ticks.map((t) => html`
          <line class="grid-line" x1=${padL} x2=${W} y1=${y(t)} y2=${y(t)} />
          <text class="axis" x=${padL - 8} y=${y(t) + 4} text-anchor="end">${format(t)}</text>`)}
        ${points.map((p, i) => {
          const top = y(p.v);
          const h = Math.max(p.v > 0 ? 2 : 0, H - padB - top);
          return html`
            <rect class="bar ${hover !== null && hover !== i ? 'dim' : ''}" x=${padL + i * bw + gap / 2} y=${H - padB - h}
              width=${Math.max(bw - gap, 1)} height=${h} rx=${Math.min(3, (bw - gap) / 3)} />
            <rect class="hit" x=${padL + i * bw} y=${padT} width=${bw} height=${H - padT - padB}
              onMouseEnter=${() => setHover(i)} />
            ${i % labelEvery === 0 && html`<text class="axis" x=${padL + i * bw + bw / 2} y=${H - 6} text-anchor="middle">${bucketLabel(p.t, step)}</text>`}`;
        })}
      </svg>
      ${hover !== null && html`
        <div class="tooltip" style=${`left:${((padL + hover * bw + bw / 2) / W) * 100}%;top:${(y(points[hover].v) / H) * 100}%`}>
          <b>${format(points[hover].v)}</b> · ${bucketLabel(points[hover].t, step, true)}
        </div>`}
    </div>`;
}

// TopList: ranked rows with an in-row magnitude bar.
export function TopList({ items, format = num, empty = 'No data in this window' }) {
  if (!items || !items.length) return html`<div class="empty small">${empty}</div>`;
  const max = Math.max(...items.map((i) => Math.abs(i.value)), 1);
  return html`
    <ol class="toplist">
      ${items.map((it) => html`
        <li title=${it.key}>
          <span class="fill" style=${`width:${(Math.abs(it.value) / max) * 100}%`}></span>
          <span class="k">${it.label || it.key}${it.label && html` <span class="faint small">${it.key}</span>`}</span>
          <span class="v">${format(it.value)}</span>
        </li>`)}
    </ol>`;
}
