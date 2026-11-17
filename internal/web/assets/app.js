/* BoxPilot web UI.
 *
 * Polls the read-only JSON API. The browser never triggers a scan: the
 * server polls in the background and the API only serves the latest
 * snapshot, so refreshing the page costs nothing upstream.
 */
'use strict';

const state = {
  rows: [],
  opp: [],
  sortKey: 'r_natural_pct',
  sortDir: 'asc',
  filter: '',
  expiry: '',
  onlyCheap: false,
  expanded: new Set(),
  cfgDirty: false,
  rowsSig: null,
  oppSig: null,
};

const $ = (id) => document.getElementById(id);

// ---------- formatting ----------
const fmtNum = (v, d = 2) =>
  (v === null || v === undefined || Number.isNaN(v)) ? '—' : Number(v).toFixed(d);
const fmtPct = (v) => (v === null || v === undefined) ? '—' : Number(v).toFixed(4) + '%';
const fmtBP = (v) => (v === null || v === undefined) ? '—' : (Number(v) >= 0 ? '+' : '') + Number(v).toFixed(1);

function fmtAge(sec) {
  if (sec === null || sec === undefined) return '—';
  const s = Math.max(0, Math.round(sec));
  if (s < 60) return s + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}

function fmtClock(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d)) return '—';
  return d.toLocaleTimeString('zh-CN', { hour12: false });
}

const esc = (s) => String(s).replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// ---------- fetching ----------
async function getJSON(url) {
  const res = await fetch(url, { headers: { 'Accept': 'application/json' } });
  if (!res.ok) throw new Error(url + ' → HTTP ' + res.status);
  return res.json();
}

// rowsSignature is a cheap checksum of the rendered content. The UI
// polls every few seconds but the data only changes when a new scan
// lands, so repainting on an unchanged signature would needlessly
// rebuild the table (detaching nodes, losing hover and selection).
function rowsSignature(rows) {
  let h = rows.length;
  for (let i = 0; i < rows.length; i++) {
    const r = rows[i];
    h = (h * 31 + r.K1 + r.K2 + Math.round(r.spread_bp * 100)) | 0;
  }
  return h;
}

async function refresh() {
  try {
    const [status, scan, opp] = await Promise.all([
      getJSON('/api/status'),
      getJSON('/api/scan'),
      getJSON('/api/opportunities'),
    ]);
    const rows = scan.rows || [];
    const opps = opp.opportunities || [];
    const rowsSig = rowsSignature(rows);
    const oppSig = rowsSignature(opps);

    state.rows = rows;
    state.opp = opps;
    renderStatus(status);
    if (oppSig !== state.oppSig) {
      state.oppSig = oppSig;
      renderOpportunities();
    }
    if (rowsSig !== state.rowsSig) {
      state.rowsSig = rowsSig;
      renderRows();
    }
    renderBanner(status);
  } catch (err) {
    showBanner('无法连接服务：' + err.message, true);
  }
}

// ---------- status bar ----------
function renderStatus(s) {
  $('s-symbol').textContent = s.symbol || '—';
  $('s-underlying').textContent = s.underlying && s.underlying !== '0.0000' ? s.underlying : '—';
  $('s-benchmark').textContent = fmtPct(s.benchmark_pct);
  $('s-counts').textContent = (s.valid ?? 0) + ' / ' + (s.total ?? 0);
  $('s-round').textContent = fmtClock(s.last_round_at) + '（' + fmtAge(s.data_age_sec) + '）';
  $('s-alerts').textContent = s.alerts_sent ?? 0;

  const fresh = $('s-fresh');
  const age = s.data_age_sec ?? 0;
  let cls = 'fresh', label = '新鲜';
  if (s.stale || age > 900) { cls = 'stale'; label = '陈旧'; }
  else if (age > 300) { cls = 'aging'; label = '偏旧'; }
  fresh.innerHTML = '<span class="dot ' + cls + '"></span>' + label;

  if (!state.cfgDirty) fillConfig(s);
}

function renderBanner(s) {
  if (s.last_error) {
    showBanner('扫描失败：' + s.last_error, true);
  } else if (s.stale) {
    showBanner('行情数据已陈旧（' + fmtAge(s.data_age_sec) + '），机会报警已挂起。', false);
  } else {
    hideBanner();
  }
}

function showBanner(msg, isError) {
  const b = $('banner');
  b.textContent = msg;
  b.classList.remove('hidden');
  b.style.background = isError ? 'rgba(248,81,73,.12)' : 'rgba(210,153,34,.12)';
  b.style.borderColor = isError ? 'rgba(248,81,73,.4)' : 'rgba(210,153,34,.4)';
  b.style.color = isError ? '#ffb4ae' : '#e3b341';
}
function hideBanner() { $('banner').classList.add('hidden'); }

// ---------- opportunities ----------
function renderOpportunities() {
  const tbody = $('opp-table').querySelector('tbody');
  const wrap = tbody.closest('.table-wrap');
  const keepScroll = wrap ? wrap.scrollTop : 0;
  $('opp-count').textContent = state.opp.length;
  if (!state.opp.length) {
    tbody.innerHTML = '<tr class="empty"><td colspan="8">暂无机会</td></tr>';
    return;
  }
  const rows = state.opp.slice().sort((a, b) => a.spread_bp - b.spread_bp);
  tbody.innerHTML = rows.map((o) => {
    const status = o.alerted
      ? '<span class="pill alerted">已报警</span>'
      : '<span class="pill wait">确认 ' + o.consecutive + ' 轮</span>';
    const cls = o.spread_bp < 0 ? 'cheap' : 'expensive';
    return '<tr>' +
      '<td>' + esc(o.expiry) + '</td>' +
      '<td class="num">' + o.D + '</td>' +
      '<td class="num">' + o.K1 + '</td>' +
      '<td class="num">' + o.K2 + '</td>' +
      '<td class="num">' + o.width + '</td>' +
      '<td class="num">' + fmtPct(o.r_natural_pct) + '</td>' +
      '<td class="num ' + cls + '">' + fmtBP(o.spread_bp) + '</td>' +
      '<td>' + status + '</td>' +
      '</tr>';
  }).join('');
  if (wrap) wrap.scrollTop = keepScroll;
}

// ---------- scan table ----------
function visibleRows() {
  let rows = state.rows.slice();
  if (state.expiry) rows = rows.filter((r) => r.expiry === state.expiry);
  if (state.onlyCheap) rows = rows.filter((r) => r.spread_bp <= 0);
  const q = state.filter.trim();
  if (q) {
    const needle = q.toLowerCase();
    rows = rows.filter((r) =>
      String(r.K1).includes(needle) || String(r.K2).includes(needle) ||
      r.expiry.toLowerCase().includes(needle));
  }
  const key = state.sortKey, dir = state.sortDir === 'asc' ? 1 : -1;
  rows.sort((a, b) => {
    const x = a[key], y = b[key];
    if (typeof x === 'number' && typeof y === 'number') return (x - y) * dir;
    return String(x).localeCompare(String(y)) * dir;
  });
  return rows;
}

// refreshExpiryOptions repopulates the expiry dropdown from the data
// currently on screen.
function refreshExpiryOptions() {
  const sel = $('expiry-filter');
  const seen = Array.from(new Set(state.rows.map((r) => r.expiry))).sort();
  const current = sel.value;
  sel.innerHTML = '<option value="">全部到期日</option>' +
    seen.map((e) => '<option value="' + esc(e) + '">' + esc(e) + '</option>').join('');
  if (seen.indexOf(current) === -1) {
    sel.value = '';
    state.expiry = '';
  } else {
    sel.value = current;
  }
}

// renderRows paints the results table. The auto-refresh replaces the
// table body every few seconds, so the scroll position of the scroll
// container is preserved — otherwise the view would jump back to the
// top while the user is reading.
function renderRows() {
  refreshExpiryOptions();
  const rows = visibleRows();
  const tbody = $('scan-table').querySelector('tbody');
  const wrap = tbody.closest('.table-wrap');
  const keepScroll = wrap ? wrap.scrollTop : 0;
  $('row-count').textContent = rows.length;
  if (!rows.length) {
    tbody.innerHTML = '<tr class="empty"><td colspan="11">没有匹配的组合</td></tr>';
    return;
  }
  let html = '';
  for (const r of rows) {
    const cls = r.spread_bp < 0 ? 'cheap' : 'expensive';
    html += '<tr class="expandable" data-key="' + r.expiry + '|' + r.K1 + '|' + r.K2 + '">' +
      '<td>' + esc(r.expiry) + '</td>' +
      '<td class="num">' + r.D + '</td>' +
      '<td class="num">' + r.K1 + '</td>' +
      '<td class="num">' + r.K2 + '</td>' +
      '<td class="num">' + r.width + '</td>' +
      '<td class="num">' + fmtNum(r.C_mid, 4) + '</td>' +
      '<td class="num">' + fmtNum(r.C_natural, 4) + '</td>' +
      '<td class="num">' + fmtPct(r.r_mid_pct) + '</td>' +
      '<td class="num">' + fmtPct(r.r_natural_pct) + '</td>' +
      '<td class="num ' + cls + '">' + fmtBP(r.spread_bp) + '</td>' +
      '<td class="num">' + r.legs_ok + '</td>' +
      '</tr>';
    if (state.expanded.has(r.expiry + '|' + r.K1 + '|' + r.K2)) {
      html += '<tr class="detail"><td colspan="11">' + legsHTML(r) + '</td></tr>';
    }
  }
  tbody.innerHTML = html;
  if (wrap) wrap.scrollTop = keepScroll;
}

function legsHTML(r) {
  const L = r.legs;
  const items = [
    ['SELL ' + L.sell_call_k1.strike + ' call', L.sell_call_k1],
    ['BUY  ' + L.buy_call_k2.strike + ' call', L.buy_call_k2],
    ['BUY  ' + L.buy_put_k1.strike + ' put', L.buy_put_k1],
    ['SELL ' + L.sell_put_k2.strike + ' put', L.sell_put_k2],
  ];
  return '<div class="legs">' + items.map(([name, q]) =>
    '<div class="leg"><b>' + esc(name) + '</b>' +
    'bid ' + fmtNum(q.bid, 4) + ' / ask ' + fmtNum(q.ask, 4) + ' / mid ' + fmtNum(q.mid, 4) +
    '</div>').join('') + '</div>';
}

// ---------- config panel ----------
// fillConfig mirrors the server's effective values into the form, but
// only while the user has no unsaved edits.
function fillConfig(s) {
  $('c-benchmark').value = Number(s.benchmark_pct / 100).toFixed(4);
  $('c-threshold').value = s.rules.threshold_bp;
  $('c-confirm').value = s.rules.confirm_rounds;
  $('c-hysteresis').value = s.rules.hysteresis_bp;
  $('c-interval').value = Math.round(s.rules.interval_sec) + 's';
  $('c-cooldown').value = Math.round(s.rules.cooldown_sec) + 's';
}

async function loadConfig() {
  try {
    const c = await getJSON('/api/config');
    window.__channels = c.watch.channels || [];
    $('c-channels').textContent = window.__channels.join(', ') || '—';
    $('c-expiries').value = (c.expiries || []).join(', ');
    $('c-benchmark').value = Number(c.benchmark_rate).toFixed(4);
    $('c-threshold').value = c.watch.threshold_bp;
    $('c-confirm').value = c.watch.confirm_rounds;
    $('c-hysteresis').value = c.watch.hysteresis_bp;
    $('c-interval').value = c.watch.interval;
    $('c-cooldown').value = c.watch.cooldown;
  } catch (err) {
    console.error('load config', err);
  }
}

async function saveConfig(ev) {
  ev.preventDefault();
  const msg = $('config-msg');
  msg.className = 'msg';
  msg.textContent = '保存中…';

  const expiries = $('c-expiries').value.split(',').map((s) => s.trim()).filter(Boolean);
  const body = {
    benchmark_rate: Number($('c-benchmark').value),
    expiries: expiries,
    threshold_bp: Number($('c-threshold').value),
    confirm_rounds: Number($('c-confirm').value),
    hysteresis_bp: Number($('c-hysteresis').value),
    interval: $('c-interval').value.trim(),
    cooldown: $('c-cooldown').value.trim(),
  };
  try {
    const res = await fetch('/api/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const data = await res.json();
    if (!res.ok) {
      msg.className = 'msg err';
      msg.textContent = data.error || ('HTTP ' + res.status);
      return;
    }
    state.cfgDirty = false;
    msg.className = 'msg ok';
    msg.textContent = '已生效';
    await loadConfig();
    refresh();
  } catch (err) {
    msg.className = 'msg err';
    msg.textContent = err.message;
  }
}

// ---------- wiring ----------
function init() {
  // sorting
  document.querySelectorAll('#scan-table thead th[data-sort]').forEach((th) => {
    th.addEventListener('click', () => {
      const key = th.dataset.sort;
      if (state.sortKey === key) {
        state.sortDir = state.sortDir === 'asc' ? 'desc' : 'asc';
      } else {
        state.sortKey = key;
        state.sortDir = (key === 'r_natural_pct' || key === 'spread_bp' || key === 'C_natural') ? 'asc' : 'desc';
      }
      document.querySelectorAll('#scan-table thead th').forEach((h) => h.classList.remove('sorted', 'desc'));
      th.classList.add('sorted');
      if (state.sortDir === 'desc') th.classList.add('desc');
      renderRows();
    });
  });

  // row expansion
  $('scan-table').querySelector('tbody').addEventListener('click', (ev) => {
    const tr = ev.target.closest('tr.expandable');
    if (!tr) return;
    const key = tr.dataset.key;
    if (state.expanded.has(key)) state.expanded.delete(key);
    else state.expanded.add(key);
    renderRows();
  });

  // filters
  $('filter').addEventListener('input', (e) => { state.filter = e.target.value; renderRows(); });
  $('only-cheap').addEventListener('change', (e) => { state.onlyCheap = e.target.checked; renderRows(); });
  $('expiry-filter').addEventListener('change', (e) => { state.expiry = e.target.value; renderRows(); });

  // config form
  $('config-form').addEventListener('submit', saveConfig);
  $('config-form').addEventListener('input', () => { state.cfgDirty = true; });

  loadConfig();
  refresh();
  setInterval(refresh, 5000);
}

document.addEventListener('DOMContentLoaded', init);
