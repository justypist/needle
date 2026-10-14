'use strict';

/* --- palette: keep in step with style.css ------------------------------- */

const C = {
  cpu: '#4c9aff',
  mem: '#a371f7',
  swap: '#d29922',
  rx: '#3fb950',
  tx: '#56d4dd',
  read: '#f0883e',
  write: '#db61a2',
  fg: '#e6edf3',
  dim: '#9aa7b4',
  mute: '#64707d',
  grid: '#1b222a',
  axis: '#232a33',
};

const CORES = ['#4c9aff', '#a371f7', '#3fb950', '#d29922', '#f85149', '#56d4dd', '#db61a2', '#f0883e'];

const RANGES = [
  { key: '1h', label: '1H', ms: 3600e3 },
  { key: '6h', label: '6H', ms: 6 * 3600e3 },
  { key: '24h', label: '24H', ms: 24 * 3600e3 },
  { key: '7d', label: '7D', ms: 7 * 86400e3 },
  { key: '30d', label: '30D', ms: 30 * 86400e3 },
  { key: '90d', label: '90D', ms: 90 * 86400e3 },
  { key: '365d', label: '1Y', ms: 365 * 86400e3 },
];

const LIST_REFRESH = 5000;
const DETAIL_REFRESH = 30000;

/* --- tiny helpers ------------------------------------------------------- */

const $ = (sel, root = document) => root.querySelector(sel);

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') node.className = v;
    else if (k === 'html') node.innerHTML = v;
    else if (k.startsWith('on')) node.addEventListener(k.slice(2).toLowerCase(), v);
    else if (v !== null && v !== undefined) node.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

async function api(path, opts) {
  const res = await fetch(path, { credentials: 'same-origin', ...opts });
  if (res.status === 401) {
    const err = new Error('unauthorized');
    err.unauthorized = true;
    throw err;
  }
  if (!res.ok) {
    let msg = res.statusText;
    try {
      msg = (await res.json()).error || msg;
    } catch (_) { /* non-JSON error body */ }
    throw new Error(msg);
  }
  return res.json();
}

const num = (v) => (typeof v === 'number' && isFinite(v) ? v : 0);

function fmtBytes(v) {
  const n = num(v);
  if (n < 1024) return n.toFixed(0) + ' B';
  const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let x = n / 1024;
  let i = 0;
  while (x >= 1024 && i < units.length - 1) { x /= 1024; i++; }
  return x.toFixed(x >= 100 ? 0 : 1) + ' ' + units[i];
}

const fmtRate = (v) => fmtBytes(v) + '/s';
const fmtPct = (v) => num(v).toFixed(1) + '%';

function fmtDur(sec) {
  const s = Math.max(0, Math.floor(num(sec)));
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s % 60}s`;
  return `${s}s`;
}

function fmtAgo(ms) {
  if (!ms) return 'never';
  const sec = (Date.now() - ms) / 1000;
  if (sec < 60) return `${Math.floor(sec)}s ago`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ago`;
  if (sec < 86400) return `${Math.floor(sec / 3600)}h ago`;
  return `${Math.floor(sec / 86400)}d ago`;
}

const fmtStep = (ms) => (ms >= 3600e3 ? `${ms / 3600e3}h` : ms >= 60e3 ? `${ms / 60e3}m` : `${ms / 1000}s`);

function bar(frac, cls) {
  const pct = Math.max(0, Math.min(1, num(frac))) * 100;
  return el('div', { class: 'bar ' + (cls || '') }, el('i', { style: `width:${pct}%` }));
}

const pctOf = (used, total) => (num(total) > 0 ? (num(used) / num(total)) * 100 : 0);

function meter(label, valueText, frac, cls) {
  return el('div', {},
    el('div', { class: 'meter-label' }, el('span', {}, label), el('span', { class: 'val' }, valueText)),
    bar(frac, cls),
  );
}

/* --- routing ------------------------------------------------------------ */

let timer = null;
let route = { name: 'list', id: null, range: '6h' };

function parseHash() {
  const h = location.hash.replace(/^#\/?/, '');
  const m = h.match(/^node\/(.+)$/);
  if (m) {
    route.name = 'detail';
    route.id = decodeURIComponent(m[1]);
  } else {
    route.name = 'list';
    route.id = null;
  }
}

function go(hash) {
  if (location.hash === hash) render();
  else location.hash = hash;
}

function stopTimer() {
  if (timer) { clearInterval(timer); timer = null; }
}

function setCrumbs(parts) {
  const c = $('#crumbs');
  c.replaceChildren();
  parts.forEach((p, i) => {
    if (i > 0) c.append(el('span', { class: 'muted' }, '/'));
    c.append(p.href ? el('a', { href: p.href }, p.text) : el('span', {}, p.text));
  });
}

/* --- login -------------------------------------------------------------- */

function renderLogin(errText) {
  stopTimer();
  setCrumbs([]);
  $('#stats').textContent = '';
  const input = el('input', { type: 'password', placeholder: 'Password', autocomplete: 'current-password', autofocus: 'autofocus' });
  const err = el('div', { class: 'err' }, errText || '');
  const form = el('form', {
    onsubmit: async (e) => {
      e.preventDefault();
      err.textContent = '';
      try {
        await api('/api/login', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ password: input.value }),
        });
        render();
      } catch (ex) {
        err.textContent = ex.message || 'Login failed';
        input.select();
      }
    },
  }, el('h1', {}, 'needle'), input, err, el('button', { type: 'submit' }, 'Sign in'));

  $('#main').replaceChildren(el('div', { class: 'login' }, form));
  input.focus();
}

/* --- node list ---------------------------------------------------------- */

async function renderList() {
  setCrumbs([{ text: 'Nodes' }]);
  let data;
  try {
    data = await api('/api/nodes');
  } catch (ex) {
    if (ex.unauthorized) return renderLogin();
    return renderError(ex);
  }

  const nodes = data.nodes || [];
  const main = $('#main');

  if (nodes.length === 0) {
    main.replaceChildren(el('div', { class: 'empty' },
      'No nodes have reported yet. Start an agent with the server URL and token.'));
  } else {
    const online = nodes.filter((n) => n.online).length;
    main.replaceChildren(
      el('div', { class: 'banner', style: 'color:var(--fg-dim);background:var(--panel);border-color:var(--line)' },
        `${nodes.length} node${nodes.length === 1 ? '' : 's'}, ${online} online`),
      el('div', { class: 'grid' }, nodes.map(nodeCard)),
    );
  }

  api('/api/stats').then((s) => {
    $('#stats').textContent = `${s.hosts} hosts · ${s.samples.toLocaleString()} samples · ${s.series.toLocaleString()} series`;
  }).catch(() => {});

  stopTimer();
  timer = setInterval(() => { if (route.name === 'list') render(); }, LIST_REFRESH);
}

function nodeCard(n) {
  const s = n.sample || {};
  const cpu = num(s.cpu);
  const memPct = pctOf(s.mem_used, s.mem_total);
  const swapPct = pctOf(s.swap_used, s.swap_total);

  const meters = [
    meter('CPU', fmtPct(cpu), cpu / 100, 'cpu'),
    meter('Memory', `${fmtBytes(s.mem_used)} / ${fmtBytes(s.mem_total)}`, memPct / 100, 'mem'),
  ];
  if (num(s.swap_total) > 0) {
    meters.push(meter('Swap', `${fmtBytes(s.swap_used)} / ${fmtBytes(s.swap_total)}`, swapPct / 100, 'swap'));
  }

  return el('a', { class: 'card', href: `#/node/${encodeURIComponent(n.id)}` },
    el('div', { class: 'card-head' },
      el('span', { class: 'dot ' + (n.online ? 'on' : 'off') }),
      el('span', { class: 'card-name' }, n.name || n.id),
      el('span', { class: 'card-os' }, n.os || ''),
    ),
    el('div', { class: 'meters' }, meters),
    el('div', { class: 'card-foot' },
      el('span', { class: 'up' }, '▲ ', el('b', {}, fmtRate(s.net_rx))),
      el('span', { class: 'down' }, '▼ ', el('b', {}, fmtRate(s.net_tx))),
      el('span', {}, 'load ', el('b', {}, num(s.load1).toFixed(2))),
      el('span', { style: 'margin-left:auto' }, 'up ', el('b', {}, fmtDur(s.uptime))),
    ),
  );
}

/* --- detail ------------------------------------------------------------- */

async function renderDetail() {
  let data;
  try {
    data = await api(`/api/node?id=${encodeURIComponent(route.id)}&range=${route.range}`);
  } catch (ex) {
    if (ex.unauthorized) return renderLogin();
    return renderError(ex);
  }

  const host = data.host;
  const latest = data.latest || {};
  const series = data.series || [];
  const range = data.range || { ts: [], cols: {}, tier: 0, step: 30000 };
  const name = host.name || host.id;

  document.title = `${name} · needle`;
  setCrumbs([{ text: 'Nodes', href: '#/' }, { text: name }]);

  const pts = (range.ts || []).length;

  const main = $('#main');
  main.replaceChildren(
    detailHeader(data),
    pts === 0
      ? el('div', { class: 'empty' }, 'No samples in this window. Try a longer range.')
      : el('div', {},
          el('div', { class: 'charts' }, ...charts(range)),
          panels(latest, series),
        ),
  );

  stopTimer();
  timer = setInterval(() => { if (route.name === 'detail') render(); }, DETAIL_REFRESH);
}

function detailHeader(data) {
  const h = data.host;
  const s = data.latest || {};
  const meta = [
    ['status', data.online ? 'online' : 'offline'],
    ['os', h.os],
    ['kernel', h.kernel],
    ['arch', h.arch],
    ['cpu', h.cpu_model ? `${h.cpu_model} (${h.cores})` : `${h.cores} cores`],
    ['uptime', fmtDur(s.uptime)],
    ['last report', fmtAgo(h.last_seen)],
  ].filter(([, v]) => v);

  const range = data.range || {};
  const pts = (range.ts || []).length;
  const tierName = ['raw', '5-min', 'hourly'][range.tier] || 'raw';

  return el('div', {},
    el('div', { class: 'detail-head' },
      el('h1', {}, el('span', { class: 'dot ' + (data.online ? 'on' : 'off'), style: 'margin-right:10px' }), h.name || h.id),
      el('div', { class: 'meta' }, meta.map(([k, v]) => el('span', {}, el('b', {}, k + ': '), v))),
    ),
    el('div', { class: 'toolbar' },
      el('div', { class: 'ranges' }, RANGES.map((r) =>
        el('button', {
          class: r.key === route.range ? 'active' : '',
          onclick: () => { route.range = r.key; render(); },
        }, r.label))),
      el('div', { class: 'spacer' }),
      el('span', { class: 'tier' }, `${pts} pts · ${tierName} · step ${fmtStep(range.step || 0)}`),
    ),
  );
}

/* --- chart construction ------------------------------------------------- */

function baseOpts(width, height, yFmt) {
  const axisBase = {
    stroke: C.mute,
    grid: { stroke: C.grid, width: 1 },
    ticks: { stroke: C.axis, width: 1 },
    font: '11px ui-monospace, monospace',
  };
  return {
    width,
    height,
    padding: [10, 12, 0, 0],
    cursor: { drag: { x: true, y: false }, points: { size: 6 } },
    legend: { show: false },
    scales: {
      x: { time: true },
      // Bars that start at zero read better for percentage and byte gauges;
      // load and temperature are happier with a tight window.
      y: { range: (u, min, max) => (yFmt.zero ? [0, max * 1.08] : [min - (max - min) * 0.05, max * 1.08]) },
    },
    axes: [
      { ...axisBase, space: 70, values: (u, vals) => vals.map(shortTime) },
      { ...axisBase, size: 62, values: (u, vals) => vals.map((v) => yFmt(v)) },
    ],
    series: [],
  };
}

// uPlot's built-in time formatting is verbose here; a compact local-time
// label is enough at every zoom level.
function shortTime(sec) {
  const d = new Date(sec * 1000);
  const span = Date.now() / 1000 - sec;
  const p = (n) => String(n).padStart(2, '0');
  if (span < 86400) return `${p(d.getHours())}:${p(d.getMinutes())}`;
  if (span < 86400 * 180) return `${d.getMonth() + 1}/${d.getDate()} ${p(d.getHours())}:${p(d.getMinutes())}`;
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

function makeChart(box, defs, ts, yFmt, height) {
  const mount = box.querySelector('.chart');
  const width = Math.max(280, mount.clientWidth || 320);
  const data = [ts.map((t) => t / 1000), ...defs.map((d) => d.data)];
  const opts = baseOpts(width, height || 180, yFmt);
  opts.series = [{}, ...defs.map((d) => ({
    label: d.label,
    stroke: d.color,
    width: d.width || 1.5,
    dash: d.dash,
    points: { show: false },
    value: (u, v) => (v == null ? '--' : yFmt(v)),
  }))];
  const u = new uPlot(opts, data, mount);
  resizeObserver(mount, u);
  return u;
}

const ro = new ResizeObserver((entries) => {
  for (const e of entries) {
    const u = e.target.__u;
    if (u) u.setSize({ width: Math.max(280, e.contentRect.width), height: u.height });
  }
});

function resizeObserver(host, u) {
  host.__u = u;
  ro.observe(host);
}

// A formatter may carry zero:true to anchor the y axis at zero, which reads
// better for percentage and byte gauges than a tight floating window.
const fmt = (fn, zero) => Object.assign(fn, { zero: !!zero });

function cols(range) {
  return range.cols || {};
}

// Series names are opaque; pick the busiest few so the plot keeps meaning
// when a host has more interfaces or disks than are worth drawing.
function topLabels(range, kind, max) {
  const c = cols(range);
  const peaks = {};
  for (const [key, arr] of Object.entries(c)) {
    const parts = key.split(':');
    if (parts[0] !== kind || parts[2] === 'b') continue;
    const label = parts[1];
    let peak = 0;
    for (const v of arr) if (v != null && v > peak) peak = v;
    peaks[label] = Math.max(peaks[label] || 0, peak);
  }
  return Object.entries(peaks).sort((a, b) => b[1] - a[1]).slice(0, max).map(([label]) => label);
}

// A series either starts with cpu/cpu<N> or with everything before the first
// colon, which is the numbering the collector uses for cores and the device
// names it uses for interfaces and disks.
function labelOf(key) {
  const parts = key.split(':');
  return parts[0] === 'cpu' ? parts[1] : parts[1] === undefined ? key : parts[1];
}

function coreIndex(label) {
  const n = parseInt(String(label).replace(/^cpu/, ''), 10);
  return isNaN(n) ? 0 : n;
}

function charts(range) {
  const c = cols(range);
  const ts = range.ts || [];
  const wraps = [];

  const add = (title, defs, yFmt, wide, height) => {
    const live = defs.filter((d) => Array.isArray(d.data) && d.data.length === ts.length);
    if (!live.length) return;
    const box = el('div', { class: 'chart-box' + (wide ? ' wide' : '') });
    const wrap = el('div', {},
      el('h2', {}, title),
      el('div', { class: 'legend' }, live.map((d) =>
        el('span', {}, el('i', { style: `background:${d.color}` }), d.label))),
      box,
    );
    box.append(el('div', { class: 'chart' }));
    wraps.push({ wrap, box, defs: live, yFmt, height: height || 175 });
  };

  const coreKeys = Object.keys(c)
    .filter((k) => k.startsWith('cpu:cpu') && k !== 'cpu:cpu')
    .sort((a, b) => coreIndex(labelOf(a)) - coreIndex(labelOf(b)));
  const cpuDefs = [{ label: 'total', color: C.cpu, data: c.cpu, width: 2 }];
  coreKeys.slice(0, 32).forEach((k, i) => {
    cpuDefs.push({ label: labelOf(k), color: CORES[i % CORES.length], data: c[k], width: 1 });
  });
  add('CPU', cpuDefs, fmt((v) => num(v).toFixed(0) + '%', true), true, 195);

  add('Memory', [
    { label: 'used', color: C.mem, data: c.mem_used, width: 1.6 },
    { label: 'cached', color: C.dim, data: c.mem_cached, dash: [4, 3] },
    { label: 'swap used', color: C.swap, data: c.swap_used },
  ], fmt(fmtBytes, true));

  add('Load average', [
    { label: 'load 1m', color: C.cpu, data: c.load1, width: 1.6 },
    { label: 'load 5m', color: C.rx, data: c.load5 },
    { label: 'load 15m', color: C.swap, data: c.load15 },
  ], fmt((v) => num(v).toFixed(2), true));

  const netDefs = [];
  topLabels(range, 'net', 4).forEach((name, i) => {
    const base = i === 0 ? C.rx : CORES[(i + 2) % CORES.length];
    netDefs.push({ label: `▲ ${name}`, color: base, data: c[`net:${name}`], width: 1.4 });
    netDefs.push({ label: `▼ ${name}`, color: base, data: c[`net:${name}:b`], dash: [4, 3], width: 1.2 });
  });
  add('Network', netDefs, fmt(fmtRate, true), true);

  const diskDefs = [];
  topLabels(range, 'disk', 4).forEach((name, i) => {
    const base = i === 0 ? C.read : CORES[(i + 4) % CORES.length];
    diskDefs.push({ label: `read ${name}`, color: base, data: c[`disk:${name}`], width: 1.4 });
    diskDefs.push({ label: `write ${name}`, color: base, data: c[`disk:${name}:b`], dash: [4, 3], width: 1.2 });
  });
  add('Disk I/O', diskDefs, fmt(fmtRate, true), true);

  const tempDefs = Object.keys(c)
    .filter((k) => k.startsWith('temp:'))
    .map((k, i) => ({ label: labelOf(k), color: CORES[i % CORES.length], data: c[k], width: 1.4 }));
  add('Temperature', tempDefs, fmt((v) => num(v).toFixed(1) + '°C'));

  // uPlot needs the host element to already have a width when it is created.
  requestAnimationFrame(() => {
    for (const { box, defs, yFmt, height } of wraps) makeChart(box, defs, ts, yFmt, height);
  });
  return wraps.map((w) => w.wrap);
}

/* --- latest-state panels ------------------------------------------------ */

function panels(latest, series) {
  const out = [];

  const fs = series.filter((s) => s.kind === 'fs').sort((a, b) => b.b - a.b);
  if (fs.length) {
    out.push(el('div', { class: 'chart-box wide', style: 'margin-top:16px' },
      el('h2', {}, 'Filesystems'),
      el('div', { class: 'mounts' }, fs.map((s) => {
        const frac = pctOf(s.a, s.b);
        return el('div', { class: 'mount' },
          meter(s.label, `${fmtBytes(s.a)} / ${fmtBytes(s.b)}  (${frac.toFixed(0)}%)`, frac / 100, frac > 90 ? 'swap' : 'mem'));
      }))));
  }

  const temps = series.filter((s) => s.kind === 'temp');
  if (temps.length) {
    out.push(el('div', { class: 'chart-box wide', style: 'margin-top:16px' },
      el('h2', {}, 'Sensors'),
      el('div', { class: 'temps' }, temps.map((s) =>
        el('div', { class: 'temp-chip' }, el('span', { class: 'n' }, s.label), el('span', { class: 'v' }, s.a.toFixed(1) + '°C'))))));
  }

  const cores = series.filter((s) => s.kind === 'cpu').sort((a, b) => coreIndex(a.label) - coreIndex(b.label));
  if (cores.length) {
    out.push(el('div', { class: 'chart-box wide', style: 'margin-top:16px' },
      el('h2', {}, `Cores (${cores.length})`),
      el('div', { class: 'cores' }, cores.map((s, i) =>
        el('div', { class: 'core' },
          el('div', { class: 'n' }, `#${i}`),
          el('div', { class: 'v' }, fmtPct(s.a)),
          bar(s.a / 100, ''))))));
  }

  return el('div', {}, out);
}

/* --- error surface ------------------------------------------------------ */

function renderError(ex) {
  stopTimer();
  $('#main').replaceChildren(el('div', { class: 'empty' },
    el('div', {}, 'Could not load data'),
    el('div', { class: 'mono', style: 'margin-top:8px;color:var(--bad)' }, ex.message || String(ex))));
}

/* --- boot --------------------------------------------------------------- */

async function render() {
  parseHash();
  if (route.name === 'detail') await renderDetail();
  else await renderList();
}

$('#logout').addEventListener('click', async () => {
  try { await api('/api/logout', { method: 'POST' }); } catch (_) { /* already out */ }
  route.name = 'list';
  location.hash = '';
  render();
});

window.addEventListener('hashchange', render);

// The dashboard is password-gated, so confirm the session exists before
// showing the node grid; a stale cookie lands on the login screen instead.
(async function boot() {
  try {
    const s = await api('/api/session');
    if (!s.auth) $('#logout').style.display = 'none';
    await render();
  } catch (ex) {
    if (ex.unauthorized) renderLogin();
    else renderError(ex);
  }
})();
