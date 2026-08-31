package httpapi

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Trojan-Go Traffic Monitor</title>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.7/dist/chart.umd.min.js"></script>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background: #0f172a; color: #e2e8f0; min-height: 100vh; }
  .header { background: #1e293b; padding: 20px 32px; border-bottom: 1px solid #334155; display: flex; align-items: center; justify-content: space-between; }
  .header h1 { font-size: 20px; font-weight: 600; color: #f1f5f9; }
  .header h1 span { color: #38bdf8; }
  .status-dot { width: 8px; height: 8px; border-radius: 50%; background: #22c55e; display: inline-block; margin-right: 8px; animation: pulse 2s infinite; }
  @keyframes pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.5; } }
  .container { max-width: 1200px; margin: 0 auto; padding: 24px; }
  .stats-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 16px; margin-bottom: 24px; }
  .stat-card { background: #1e293b; border-radius: 12px; padding: 20px; border: 1px solid #334155; }
  .stat-card .label { font-size: 12px; color: #94a3b8; text-transform: uppercase; letter-spacing: 0.05em; margin-bottom: 8px; }
  .stat-card .value { font-size: 28px; font-weight: 700; color: #f1f5f9; }
  .stat-card .unit { font-size: 14px; color: #64748b; margin-left: 4px; }
  .stat-card .sub { font-size: 12px; color: #94a3b8; margin-top: 6px; }
  .metrics-grid { display: grid; grid-template-columns: repeat(2, 1fr); gap: 16px; margin-bottom: 24px; }
  .metric-card { background: #1e293b; border-radius: 12px; padding: 20px; border: 1px solid #334155; }
  .metric-card h3 { font-size: 13px; font-weight: 600; color: #94a3b8; text-transform: uppercase; letter-spacing: 0.05em; margin-bottom: 14px; }
  .metric-row { display: flex; justify-content: space-between; align-items: center; padding: 6px 0; font-size: 13px; }
  .metric-row .k { color: #94a3b8; }
  .metric-row .v { color: #f1f5f9; font-weight: 600; }
  .metric-row .v.up { color: #38bdf8; }
  .metric-row .v.down { color: #a78bfa; }
  .metric-row .v.warn { color: #fbbf24; }
  .metric-row .v.err { color: #f87171; }
  .bar { width: 100%; height: 6px; background: #0f172a; border-radius: 3px; overflow: hidden; margin-top: 4px; }
  .bar > div { height: 100%; transition: width 0.3s; }
  .bar.lo > div { background: #22c55e; }
  .bar.md > div { background: #fbbf24; }
  .bar.hi > div { background: #f87171; }
  @media (max-width: 768px) { .metrics-grid { grid-template-columns: 1fr; } }
  .chart-container { background: #1e293b; border-radius: 12px; padding: 24px; border: 1px solid #334155; margin-bottom: 24px; }
  .chart-container h2 { font-size: 16px; font-weight: 600; margin-bottom: 16px; color: #f1f5f9; }
  .chart-wrap { position: relative; height: 320px; }
  .conn-table { background: #1e293b; border-radius: 12px; border: 1px solid #334155; overflow: hidden; }
  .conn-table h2 { font-size: 16px; font-weight: 600; padding: 20px 24px 16px; color: #f1f5f9; }
  table { width: 100%; border-collapse: collapse; }
  th { text-align: left; padding: 10px 16px; font-size: 11px; text-transform: uppercase; letter-spacing: 0.05em; color: #64748b; background: #0f172a; border-bottom: 1px solid #334155; }
  td { padding: 12px 16px; font-size: 13px; border-bottom: 1px solid #1e293b; color: #cbd5e1; }
  tr:hover td { background: #1e293b80; }
  .badge { padding: 2px 8px; border-radius: 9999px; font-size: 11px; font-weight: 600; }
  .badge-active { background: #064e3b; color: #34d399; }
  .badge-closed { background: #44403c; color: #a8a29e; }
  .up-color { color: #38bdf8; }
  .down-color { color: #a78bfa; }
  .logout-btn { background: none; border: 1px solid #475569; color: #94a3b8; padding: 6px 14px; border-radius: 6px; cursor: pointer; font-size: 12px; }
  .logout-btn:hover { border-color: #64748b; color: #e2e8f0; }
  @media (max-width: 768px) { .stats-grid { grid-template-columns: repeat(2, 1fr); } }

  /* Login overlay */
  #loginOverlay { position: fixed; inset: 0; background: #0f172a; z-index: 1000; display: flex; align-items: center; justify-content: center; }
  .login-box { background: #1e293b; border: 1px solid #334155; border-radius: 16px; padding: 40px; width: 100%; max-width: 400px; text-align: center; }
  .login-box h2 { font-size: 22px; font-weight: 700; margin-bottom: 8px; color: #f1f5f9; }
  .login-box h2 span { color: #38bdf8; }
  .login-box p { font-size: 14px; color: #64748b; margin-bottom: 24px; }
  .login-box input { width: 100%; padding: 12px 16px; background: #0f172a; border: 1px solid #334155; border-radius: 8px; color: #f1f5f9; font-size: 15px; outline: none; margin-bottom: 16px; }
  .login-box input:focus { border-color: #38bdf8; }
  .login-box button { width: 100%; padding: 12px; background: #0ea5e9; border: none; border-radius: 8px; color: #fff; font-size: 15px; font-weight: 600; cursor: pointer; }
  .login-box button:hover { background: #0284c7; }
  .login-error { color: #f87171; font-size: 13px; margin-bottom: 12px; min-height: 20px; }
</style>
</head>
<body>

<!-- Login overlay -->
<div id="loginOverlay">
  <div class="login-box">
    <h2><span>Trojan-Go</span> Monitor</h2>
    <p>Enter the secret key to access the dashboard</p>
    <div class="login-error" id="loginError"></div>
    <input type="password" id="secretInput" placeholder="Secret key" autofocus>
    <button onclick="doLogin()">Authenticate</button>
  </div>
</div>

<!-- Dashboard content -->
<div id="dashboardContent" style="display:none">
<div class="header">
  <h1><span>Trojan-Go</span> Traffic Monitor</h1>
  <div style="display:flex;align-items:center;gap:16px">
    <div style="font-size:13px;color:#94a3b8"><span class="status-dot"></span>Live &mdash; refreshing every 2s</div>
    <button class="logout-btn" onclick="doLogout()">Logout</button>
  </div>
</div>
<div class="container">
  <div class="stats-grid">
    <div class="stat-card">
      <div class="label">Active Connections</div>
      <div class="value" id="activeConns">0</div>
      <div class="sub"><span id="openCps">0</span>/s open · <span id="closeCps">0</span>/s close</div>
    </div>
    <div class="stat-card">
      <div class="label">Total Connections (cumulative)</div>
      <div class="value" id="totalConns">0</div>
      <div class="sub">opens: <span id="connOpenTotal">0</span> · closes: <span id="connCloseTotal">0</span></div>
    </div>
    <div class="stat-card">
      <div class="label">Upload Speed</div>
      <div class="value up-color" id="totalUp">0<span class="unit">B/s</span></div>
      <div class="sub">P50 <span id="upP50">0</span> · P95 <span id="upP95">0</span></div>
    </div>
    <div class="stat-card">
      <div class="label">Download Speed</div>
      <div class="value down-color" id="totalDown">0<span class="unit">B/s</span></div>
      <div class="sub">P50 <span id="downP50">0</span> · P95 <span id="downP95">0</span></div>
    </div>
  </div>
  <div class="chart-container">
    <h2>Traffic History (Last 15 Minutes)</h2>
    <div class="chart-wrap"><canvas id="trafficChart"></canvas></div>
  </div>
  <div class="metrics-grid">
    <div class="metric-card">
      <h3>Connection Close Reasons</h3>
      <div id="closeReasons"></div>
    </div>
    <div class="metric-card">
      <h3>TLS Handshake</h3>
      <div class="metric-row"><span class="k">Total</span><span class="v" id="tlsTotal">0</span></div>
      <div class="metric-row"><span class="k">Failed</span><span class="v err" id="tlsFailed">0</span></div>
      <div class="metric-row"><span class="k">Failure Rate</span><span class="v" id="tlsFailRate">0%</span></div>
      <div class="metric-row"><span class="k">Latency P50</span><span class="v" id="tlsP50">0 ms</span></div>
      <div class="metric-row"><span class="k">Latency P95</span><span class="v" id="tlsP95">0 ms</span></div>
      <div class="metric-row"><span class="k">Resumed</span><span class="v" id="tlsResumed">0</span></div>
    </div>
    <div class="metric-card">
      <h3>Origin Dial (server &rarr; upstream)</h3>
      <div class="metric-row"><span class="k">Total</span><span class="v" id="originTotal">0</span></div>
      <div class="metric-row"><span class="k">Failed</span><span class="v err" id="originFailed">0</span></div>
      <div class="metric-row"><span class="k">Failure Rate</span><span class="v" id="originFailRate">0%</span></div>
      <div class="metric-row"><span class="k">Latency P50</span><span class="v" id="originP50">0 ms</span></div>
      <div class="metric-row"><span class="k">Latency P95</span><span class="v" id="originP95">0 ms</span></div>
      <div style="margin-top:10px;font-size:11px;color:#64748b;text-transform:uppercase;letter-spacing:0.05em">Latency Split</div>
      <div class="metric-row"><span class="k">DNS P50</span><span class="v" id="dnsP50">0 ms</span></div>
      <div class="metric-row"><span class="k">DNS P95</span><span class="v" id="dnsP95">0 ms</span></div>
      <div class="metric-row"><span class="k">TCP Dial P50</span><span class="v" id="tcpDialP50">0 ms</span></div>
      <div class="metric-row"><span class="k">TCP Dial P95</span><span class="v" id="tcpDialP95">0 ms</span></div>
      <div style="margin-top:10px;font-size:11px;color:#64748b;text-transform:uppercase;letter-spacing:0.05em">By Failure Kind</div>
      <div id="originFailKinds"></div>
    </div>
    <div class="metric-card">
      <h3>TTFB (dial &rarr; first byte)</h3>
      <div class="metric-row"><span class="k">Latency P50</span><span class="v" id="ttfbP50">0 ms</span></div>
      <div class="metric-row"><span class="k">Latency P95</span><span class="v" id="ttfbP95">0 ms</span></div>
      <div style="margin-top:10px;font-size:12px;color:#64748b;line-height:1.4">Higher TTFB with normal dial latency typically points to a slow origin application, not network.</div>
    </div>
    <div class="metric-card">
      <h3>Trojan Auth</h3>
      <div class="metric-row"><span class="k">Total</span><span class="v" id="authTotal">0</span></div>
      <div class="metric-row"><span class="k">Failed</span><span class="v err" id="authFailed">0</span></div>
      <div class="metric-row"><span class="k">Failure Rate</span><span class="v" id="authFailRate">0%</span></div>
      <div class="metric-row"><span class="k">Latency P50</span><span class="v" id="authP50">0 ms</span></div>
      <div class="metric-row"><span class="k">Latency P95</span><span class="v" id="authP95">0 ms</span></div>
      <div style="margin-top:10px;font-size:11px;color:#64748b;text-transform:uppercase;letter-spacing:0.05em">By Failure Kind</div>
      <div id="authFailKinds"></div>
    </div>
    <div class="metric-card">
      <h3>Channel Water-marks</h3>
      <div id="channelGauges"></div>
    </div>
    <div class="metric-card">
      <h3>Go Runtime</h3>
      <div class="metric-row"><span class="k">Goroutines</span><span class="v" id="rtGoroutines">0</span></div>
      <div class="metric-row"><span class="k">Heap Alloc</span><span class="v" id="rtHeapAlloc">0 MB</span></div>
      <div class="metric-row"><span class="k">Heap Sys</span><span class="v" id="rtHeapSys">0 MB</span></div>
      <div class="metric-row"><span class="k">Num GC</span><span class="v" id="rtNumGC">0</span></div>
      <div class="metric-row"><span class="k">Last GC Pause</span><span class="v" id="rtGcPause">0 ms</span></div>
      <div class="metric-row"><span class="k">GC CPU%</span><span class="v" id="rtGcCpu">0%</span></div>
    </div>
  </div>

  <!-- Phase 1+2+3: Pool & UDP section -->
  <h2 style="margin:24px 0 16px;font-size:16px;font-weight:600;color:#f1f5f9">Pool &amp; UDP</h2>
  <div class="metrics-grid">
    <div class="metric-card">
      <h3>Buffer Pools</h3>
      <div id="poolRows"><div class="metric-row"><span class="k" style="color:#64748b">n/a</span></div></div>
    </div>
    <div class="metric-card">
      <h3>UDP / Packet Flow</h3>
      <div class="metric-row"><span class="k">Open</span><span class="v" id="pktOpen">0</span></div>
      <div class="metric-row"><span class="k">Close</span><span class="v" id="pktClose">0</span></div>
      <div class="metric-row"><span class="k">PPS P50</span><span class="v" id="pktPpsP50">0</span></div>
      <div class="metric-row"><span class="k">PPS P95</span><span class="v" id="pktPpsP95">0</span></div>
      <div class="metric-row"><span class="k">BPS P50</span><span class="v up" id="pktBpsP50">0</span></div>
      <div class="metric-row"><span class="k">BPS P95</span><span class="v up" id="pktBpsP95">0</span></div>
      <div style="margin-top:10px;font-size:11px;color:#64748b;text-transform:uppercase">By Close Reason</div>
      <div id="pktCloseReasons"></div>
    </div>
  </div>

  <!-- Phase 3: Upstream & Topology section -->
  <h2 style="margin:24px 0 16px;font-size:16px;font-weight:600;color:#f1f5f9">Upstream &amp; Topology</h2>
  <div class="metrics-grid">
    <div class="metric-card" style="grid-column:span 2">
      <h3>Top Upstream Targets (by bytes down)</h3>
      <table style="font-size:12px;width:100%">
        <thead><tr><th>Host</th><th>Dials</th><th>Fail%</th><th>Dial P50</th><th>Up</th><th>Down</th></tr></thead>
        <tbody id="topTargetsBody"><tr><td colspan="6" style="text-align:center;color:#64748b">no data</td></tr></tbody>
      </table>
    </div>
    <div class="metric-card" style="grid-column:span 2">
      <h3>Top Users (by bytes down)</h3>
      <table style="font-size:12px;width:100%">
        <thead><tr><th>Hash</th><th>Conns</th><th>Auth Fail</th><th>Up</th><th>Down</th></tr></thead>
        <tbody id="topUsersBody"><tr><td colspan="5" style="text-align:center;color:#64748b">no data</td></tr></tbody>
      </table>
    </div>
  </div>

  <!-- Phase 3: System Health section -->
  <h2 style="margin:24px 0 16px;font-size:16px;font-weight:600;color:#f1f5f9">System Health</h2>
  <div class="metrics-grid">
    <div class="metric-card">
      <h3>Backpressure</h3>
      <div class="metric-row"><span class="k">Accept Drops</span><span class="v" id="acceptDrops">0</span></div>
      <div class="metric-row"><span class="k">BP Events</span><span class="v" id="bpEvents">0</span></div>
    </div>
    <div class="metric-card">
      <h3>Multiplexer</h3>
      <div class="metric-row"><span class="k">Streams Active</span><span class="v" id="muxActive">0</span></div>
      <div class="metric-row"><span class="k">Streams Total</span><span class="v" id="muxTotal">0</span></div>
      <div class="metric-row"><span class="k">Streams/Conn P50</span><span class="v" id="muxPerConnP50">0</span></div>
      <div class="metric-row"><span class="k">Streams/Conn P95</span><span class="v" id="muxPerConnP95">0</span></div>
    </div>
  </div>

  <!-- Cluster Routing Optimization -->
  <div id="clusterSection" style="display:none">
    <h2 style="margin:24px 0 16px;font-size:16px;font-weight:600;color:#f1f5f9;display:flex;align-items:center;gap:12px">
      Cluster Routing Optimization
      <span id="clModeBadge" style="display:none;padding:3px 10px;border-radius:9999px;font-size:11px;font-weight:700;letter-spacing:0.05em"></span>
    </h2>
    <div class="stats-grid" style="grid-template-columns:repeat(4,1fr)">
      <div class="stat-card">
        <div class="label">Local Node</div>
        <div class="value" id="clLocalNode" style="font-size:18px">-</div>
        <div class="sub">probe interval: <span id="clProbeInterval">-</span>s</div>
      </div>
      <div class="stat-card">
        <div class="label">Total Relays</div>
        <div class="value" id="clTotalRelays">0</div>
        <div class="sub">fallbacks: <span id="clTotalFallbacks">0</span></div>
      </div>
      <div class="stat-card" id="clGainCard">
        <div class="label" id="clGainLabel">Avg Gain</div>
        <div class="value" style="color:#22c55e" id="clAvgGain">0<span class="unit">ms</span></div>
        <div class="sub">last probe: <span id="clLastProbe">-</span></div>
      </div>
      <div class="stat-card">
        <div class="label">Probe Targets</div>
        <div class="value" id="clProbeTargets">0</div>
        <div class="sub">static: <span id="clStaticTargets">0</span> &middot; dynamic: <span id="clDynamicTargets">0</span></div>
      </div>
    </div>
    <div class="metrics-grid">
      <div class="metric-card" id="clPeersCard" style="grid-column:span 2">
        <h3>Master &rarr; Peer Connections</h3>
        <table style="font-size:12px;width:100%">
          <thead id="clPeersHead">
            <tr><th>Name</th><th>Host</th><th>Status</th><th>Latency</th><th>Active</th><th>Relays</th><th>Fallbacks</th></tr>
          </thead>
          <tbody id="clPeersBody"><tr><td colspan="7" style="text-align:center;color:#64748b">no peers</td></tr></tbody>
        </table>
      </div>
      <div class="metric-card" id="clRoutesCard">
        <h3>Optimized Routes</h3>
        <table style="font-size:12px;width:100%">
          <thead><tr><th>Target</th><th>Local RTT</th><th>Best Peer</th><th>Peer RTT</th><th>Gain</th></tr></thead>
          <tbody id="clRoutesBody"><tr><td colspan="5" style="text-align:center;color:#64748b">no optimized routes</td></tr></tbody>
        </table>
      </div>
    </div>
  </div>

  <div class="conn-table">
    <h2>Active Connections</h2>
    <table>
      <thead><tr><th>ID</th><th>Target</th><th>Upload Speed</th><th>Download Speed</th><th>Total Upload</th><th>Total Download</th><th>Duration</th><th>Status</th></tr></thead>
      <tbody id="connBody"></tbody>
    </table>
  </div>
</div>
</div>

<script>
var authToken = localStorage.getItem('trojan_monitor_token') || '';
var refreshTimer = null;

// --- Login ---
function doLogin() {
  var input = document.getElementById('secretInput');
  var secret = input.value.trim();
  if (!secret) { document.getElementById('loginError').textContent = 'Please enter the secret key'; return; }
  fetch('/api/auth', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ secret: secret })
  }).then(function(r) { return r.json().then(function(d) { return { ok: r.ok, data: d }; }); })
  .then(function(res) {
    if (res.ok && res.data.ok) {
      authToken = secret;
      localStorage.setItem('trojan_monitor_token', secret);
      showDashboard();
    } else {
      document.getElementById('loginError').textContent = 'Invalid secret key';
      input.value = '';
      input.focus();
    }
  }).catch(function(e) {
    document.getElementById('loginError').textContent = 'Connection error';
  });
}
document.getElementById('secretInput').addEventListener('keydown', function(e) {
  if (e.key === 'Enter') doLogin();
});

function doLogout() {
  authToken = '';
  localStorage.removeItem('trojan_monitor_token');
  if (refreshTimer) clearInterval(refreshTimer);
  document.getElementById('dashboardContent').style.display = 'none';
  document.getElementById('loginOverlay').style.display = 'flex';
  document.getElementById('secretInput').value = '';
  document.getElementById('loginError').textContent = '';
}

function showDashboard() {
  document.getElementById('loginOverlay').style.display = 'none';
  document.getElementById('dashboardContent').style.display = 'block';
  initChart();
  fetchAll();
  refreshTimer = setInterval(fetchAll, 2000);
}

// Auto-login if token exists
if (authToken) {
  fetch('/api/auth', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ secret: authToken })
  }).then(function(r) {
    if (r.ok) { showDashboard(); }
    else { authToken = ''; localStorage.removeItem('trojan_monitor_token'); }
  }).catch(function() {});
}

// --- Formatters ---
function fmtSpeed(bps) {
  if (bps < 1024) return bps.toFixed(0) + ' B/s';
  if (bps < 1048576) return (bps / 1024).toFixed(1) + ' KB/s';
  if (bps < 1073741824) return (bps / 1048576).toFixed(2) + ' MB/s';
  return (bps / 1073741824).toFixed(2) + ' GB/s';
}
function fmtBytes(b) {
  if (b < 1024) return b + ' B';
  if (b < 1048576) return (b / 1024).toFixed(1) + ' KB';
  if (b < 1073741824) return (b / 1048576).toFixed(2) + ' MB';
  return (b / 1073741824).toFixed(2) + ' GB';
}
function fmtDuration(s) {
  s = Math.floor(s);
  var h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  if (h > 0) return h + 'h ' + m + 'm ' + sec + 's';
  if (m > 0) return m + 'm ' + sec + 's';
  return sec + 's';
}
function fmtTime(ts) {
  var d = new Date(ts * 1000);
  return d.getHours().toString().padStart(2,'0') + ':' + d.getMinutes().toString().padStart(2,'0') + ':' + d.getSeconds().toString().padStart(2,'0');
}

// --- Chart ---
var chart = null;
function initChart() {
  if (chart) return;
  var ctx = document.getElementById('trafficChart').getContext('2d');
  chart = new Chart(ctx, {
    type: 'line',
    data: {
      labels: [],
      datasets: [
        { label: 'Upload', data: [], borderColor: '#38bdf8', backgroundColor: 'rgba(56,189,248,0.08)', fill: true, tension: 0.3, pointRadius: 0, borderWidth: 2 },
        { label: 'Download', data: [], borderColor: '#a78bfa', backgroundColor: 'rgba(167,139,250,0.08)', fill: true, tension: 0.3, pointRadius: 0, borderWidth: 2 }
      ]
    },
    options: {
      responsive: true, maintainAspectRatio: false,
      interaction: { mode: 'index', intersect: false },
      plugins: {
        legend: { labels: { color: '#94a3b8', usePointStyle: true, padding: 20 } },
        tooltip: {
          backgroundColor: '#1e293b', titleColor: '#f1f5f9', bodyColor: '#cbd5e1', borderColor: '#334155', borderWidth: 1,
          callbacks: { label: function(c) { return c.dataset.label + ': ' + fmtSpeed(c.raw); } }
        }
      },
      scales: {
        x: { ticks: { color: '#475569', maxTicksLimit: 15, maxRotation: 0 }, grid: { color: '#1e293b' } },
        y: { ticks: { color: '#475569', callback: function(v) { return fmtSpeed(v); } }, grid: { color: '#1e293b33' }, beginAtZero: true }
      }
    }
  });
}

function authHeaders() {
  var h = {};
  if (authToken) h['Authorization'] = 'Bearer ' + authToken;
  return h;
}

function updateChart(history) {
  if (!chart) return;
  var labels = [], up = [], down = [];
  for (var i = 0; i < history.length; i++) {
    labels.push(fmtTime(history[i].timestamp));
    up.push(history[i].upload_speed);
    down.push(history[i].download_speed);
  }
  chart.data.labels = labels;
  chart.data.datasets[0].data = up;
  chart.data.datasets[1].data = down;
  chart.update('none');
}

function updateSummary(s) {
  document.getElementById('activeConns').textContent = s.active_connections;
  document.getElementById('totalConns').textContent = s.total_connections;
  var upEl = document.getElementById('totalUp');
  var downEl = document.getElementById('totalDown');
  upEl.innerHTML = fmtSpeed(s.total_upload_speed).replace(/([\d.]+)\s*(.+)/, '$1<span class="unit">$2</span>');
  downEl.innerHTML = fmtSpeed(s.total_download_speed).replace(/([\d.]+)\s*(.+)/, '$1<span class="unit">$2</span>');
}

// --- Metrics panel update (ROI Top-5) ---
function renderCloseReasons(elId, map) {
  if (!map) return;
  var order = ['eof', 'timeout', 'reset', 'auth_fail', 'other'];
  var colors = { eof: '', timeout: 'warn', reset: 'err', auth_fail: 'err', other: '' };
  var html = '';
  for (var i = 0; i < order.length; i++) {
    var k = order[i];
    var v = map[k];
    if (v === undefined) continue;
    html += '<div class="metric-row"><span class="k">' + k + '</span><span class="v ' + (colors[k] || '') + '">' + v + '</span></div>';
  }
  document.getElementById(elId).innerHTML = html || '<div class="metric-row"><span class="k" style="color:#64748b">no data</span></div>';
}

function renderPools(pools) {
  var el = document.getElementById('poolRows');
  if (!pools || !pools.length) { el.innerHTML = '<div class="metric-row"><span class="k" style="color:#64748b">n/a</span></div>'; return; }
  el.innerHTML = pools.map(function(p) {
    var cls = p.hit_rate >= 0.9 ? 'v' : (p.hit_rate >= 0.7 ? 'v warn' : 'v err');
    return '<div style="margin-bottom:10px">' +
      '<div style="font-size:12px;color:#64748b;text-transform:uppercase">' + p.name + '</div>' +
      '<div class="metric-row"><span class="k">Hit Rate</span><span class="' + cls + '">' + (p.hit_rate * 100).toFixed(1) + '%</span></div>' +
      '<div class="metric-row"><span class="k">Gets / Puts</span><span class="v">' + (p.gets || 0) + ' / ' + (p.puts || 0) + '</span></div>' +
    '</div>';
  }).join('');
}

function renderTopTargets(list) {
  var el = document.getElementById('topTargetsBody');
  if (!list || !list.length) { el.innerHTML = '<tr><td colspan="6" style="text-align:center;color:#64748b">no data</td></tr>'; return; }
  el.innerHTML = list.slice(0, 10).map(function(t) {
    var total = t.dials || t.dial_total || 0;
    var fails = t.dial_fails || t.dial_fail_total || 0;
    var fail = total > 0 ? (fails * 100 / total) : 0;
    var cls = fail > 10 ? 'err' : (fail > 3 ? 'warn' : '');
    return '<tr>' +
      '<td style="font-family:ui-monospace,monospace">' + (t.host || '') + '</td>' +
      '<td>' + total + '</td>' +
      '<td class="' + cls + '">' + fail.toFixed(1) + '%</td>' +
      '<td>' + (t.dial_p50_ms || 0).toFixed(1) + ' ms</td>' +
      '<td class="up-color">' + fmtBytes(t.bytes_up || t.bytes_up_total || 0) + '</td>' +
      '<td class="down-color">' + fmtBytes(t.bytes_down || t.bytes_down_total || 0) + '</td>' +
    '</tr>';
  }).join('');
}

function renderTopUsers(list) {
  var el = document.getElementById('topUsersBody');
  if (!list || !list.length) { el.innerHTML = '<tr><td colspan="5" style="text-align:center;color:#64748b">no data</td></tr>'; return; }
  el.innerHTML = list.slice(0, 10).map(function(u) {
    return '<tr>' +
      '<td style="font-family:ui-monospace,monospace">' + (u.hash || '').substring(0, 12) + '...</td>' +
      '<td>' + (u.conns_total || 0) + '</td>' +
      '<td>' + (u.auth_fail_total || 0) + '</td>' +
      '<td class="up-color">' + fmtBytes(u.bytes_up_total || 0) + '</td>' +
      '<td class="down-color">' + fmtBytes(u.bytes_down_total || 0) + '</td>' +
    '</tr>';
  }).join('');
}

function updateMetrics(m) {
  if (!m) return;

  // (1) connection rate + lifecycle counters
  document.getElementById('openCps').textContent = m.open_cps || 0;
  document.getElementById('closeCps').textContent = m.close_cps || 0;
  document.getElementById('connOpenTotal').textContent = m.conn_open_total || 0;
  document.getElementById('connCloseTotal').textContent = m.conn_close_total || 0;

  // (3) throughput percentiles
  document.getElementById('upP50').textContent = fmtSpeed(m.up_bps_p50 || 0);
  document.getElementById('upP95').textContent = fmtSpeed(m.up_bps_p95 || 0);
  document.getElementById('downP50').textContent = fmtSpeed(m.down_bps_p50 || 0);
  document.getElementById('downP95').textContent = fmtSpeed(m.down_bps_p95 || 0);

  // (2) close reason breakdown
  var rmap = m.close_by_reason || {};
  var order = ['eof', 'timeout', 'reset', 'auth_fail', 'other'];
  var colors = { eof: '', timeout: 'warn', reset: 'err', auth_fail: 'err', other: '' };
  var html = '';
  for (var i = 0; i < order.length; i++) {
    var k = order[i];
    var v = rmap[k] || 0;
    html += '<div class="metric-row"><span class="k">' + k + '</span><span class="v ' + (colors[k] || '') + '">' + v + '</span></div>';
  }
  document.getElementById('closeReasons').innerHTML = html;

  // (4) TLS handshake
  var tt = m.tls_handshake_total || 0;
  var tf = m.tls_handshake_failed || 0;
  document.getElementById('tlsTotal').textContent = tt;
  document.getElementById('tlsFailed').textContent = tf;
  document.getElementById('tlsFailRate').textContent = tt > 0 ? ((tf * 100 / tt).toFixed(2) + '%') : '0%';
  document.getElementById('tlsP50').textContent = (m.tls_handshake_p50_ms || 0).toFixed(1) + ' ms';
  document.getElementById('tlsP95').textContent = (m.tls_handshake_p95_ms || 0).toFixed(1) + ' ms';

  // (4b) Origin dial — server -> upstream
  var ot = m.origin_dial_total || 0;
  var of = m.origin_dial_failed || 0;
  document.getElementById('originTotal').textContent = ot;
  document.getElementById('originFailed').textContent = of;
  document.getElementById('originFailRate').textContent = ot > 0 ? ((of * 100 / ot).toFixed(2) + '%') : '0%';
  document.getElementById('originP50').textContent = (m.origin_dial_p50_ms || 0).toFixed(1) + ' ms';
  document.getElementById('originP95').textContent = (m.origin_dial_p95_ms || 0).toFixed(1) + ' ms';
  var ofk = m.origin_dial_fail_by_kind || {};
  var ofkOrder = ['dns', 'refused', 'timeout', 'unreachable', 'other'];
  var ofkColors = { dns: 'warn', refused: 'err', timeout: 'warn', unreachable: 'err', other: '' };
  var ofkHtml = '';
  for (var i = 0; i < ofkOrder.length; i++) {
    var k = ofkOrder[i];
    var v = ofk[k] || 0;
    ofkHtml += '<div class="metric-row"><span class="k">' + k + '</span><span class="v ' + (ofkColors[k] || '') + '">' + v + '</span></div>';
  }
  document.getElementById('originFailKinds').innerHTML = ofkHtml;

  // (4c) TTFB — dial-success -> first downstream byte
  document.getElementById('ttfbP50').textContent = (m.ttfb_p50_ms || 0).toFixed(1) + ' ms';
  document.getElementById('ttfbP95').textContent = (m.ttfb_p95_ms || 0).toFixed(1) + ' ms';

  // (4d) Trojan auth
  var at = m.trojan_auth_total || 0;
  var af = m.trojan_auth_failed || 0;
  document.getElementById('authTotal').textContent = at;
  document.getElementById('authFailed').textContent = af;
  document.getElementById('authFailRate').textContent = at > 0 ? ((af * 100 / at).toFixed(2) + '%') : '0%';
  document.getElementById('authP50').textContent = (m.trojan_auth_p50_ms || 0).toFixed(1) + ' ms';
  document.getElementById('authP95').textContent = (m.trojan_auth_p95_ms || 0).toFixed(1) + ' ms';
  var afk = m.trojan_auth_fail_by_kind || {};
  var afkOrder = ['invalid_hash', 'ip_limit', 'read_hash', 'read_crlf', 'read_metadata', 'parse_host', 'other'];
  var afkColors = { invalid_hash: 'err', ip_limit: 'warn', read_hash: '', read_crlf: '', read_metadata: '', parse_host: '', other: '' };
  var afkHtml = '';
  for (var i = 0; i < afkOrder.length; i++) {
    var k = afkOrder[i];
    if (afk[k] === undefined && k !== 'invalid_hash' && k !== 'ip_limit') continue;
    var v = afk[k] || 0;
    afkHtml += '<div class="metric-row"><span class="k">' + k + '</span><span class="v ' + (afkColors[k] || '') + '">' + v + '</span></div>';
  }
  // Surface any unexpected kinds the server reports beyond the well-known set.
  for (var k in afk) {
    if (afkOrder.indexOf(k) === -1) {
      afkHtml += '<div class="metric-row"><span class="k">' + k + '</span><span class="v">' + afk[k] + '</span></div>';
    }
  }
  document.getElementById('authFailKinds').innerHTML = afkHtml;

  // channel water-marks (back-pressure visualization)
  var chans = m.channels || [];
  var chHtml = '';
  for (var i = 0; i < chans.length; i++) {
    var c = chans[i];
    var pct = c.cap > 0 ? Math.min(100, c.depth * 100 / c.cap) : 0;
    var cls = pct < 50 ? 'lo' : (pct < 80 ? 'md' : 'hi');
    chHtml += '<div class="metric-row"><span class="k">' + c.name + '</span><span class="v">' + c.depth + ' / ' + c.cap + '</span></div>'
      + '<div class="bar ' + cls + '"><div style="width:' + pct.toFixed(0) + '%"></div></div>';
  }
  document.getElementById('channelGauges').innerHTML = chHtml || '<div class="metric-row"><span class="k" style="color:#64748b">no channels registered</span></div>';

  // (5) Go runtime
  document.getElementById('rtGoroutines').textContent = m.goroutines || 0;
  document.getElementById('rtHeapAlloc').textContent = (m.heap_alloc_mb || 0).toFixed(2) + ' MB';
  document.getElementById('rtHeapSys').textContent = (m.heap_sys_mb || 0).toFixed(2) + ' MB';
  document.getElementById('rtNumGC').textContent = m.num_gc || 0;
  document.getElementById('rtGcPause').textContent = (m.gc_pause_last_ms || 0).toFixed(3) + ' ms';
  document.getElementById('rtGcCpu').textContent = ((m.gc_cpu_fraction || 0) * 100).toFixed(3) + '%';

  // Phase 1: Buffer pools
  renderPools(m.pools);

  // Phase 1: UDP/Packet flow
  document.getElementById('pktOpen').textContent = m.packet_open_total || 0;
  document.getElementById('pktClose').textContent = m.packet_close_total || 0;
  document.getElementById('pktPpsP50').textContent = (m.packet_pps_p50 || 0).toFixed(0);
  document.getElementById('pktPpsP95').textContent = (m.packet_pps_p95 || 0).toFixed(0);
  document.getElementById('pktBpsP50').textContent = fmtSpeed(m.packet_bps_p50 || 0);
  document.getElementById('pktBpsP95').textContent = fmtSpeed(m.packet_bps_p95 || 0);
  renderCloseReasons('pktCloseReasons', m.packet_close_by_reason);

  // Phase 1: DNS/TCP dial split
  document.getElementById('dnsP50').textContent = (m.dns_resolve_p50_ms || 0).toFixed(1) + ' ms';
  document.getElementById('dnsP95').textContent = (m.dns_resolve_p95_ms || 0).toFixed(1) + ' ms';
  document.getElementById('tcpDialP50').textContent = (m.tcp_dial_p50_ms || 0).toFixed(1) + ' ms';
  document.getElementById('tcpDialP95').textContent = (m.tcp_dial_p95_ms || 0).toFixed(1) + ' ms';

  // Phase 2: TLS resumption
  document.getElementById('tlsResumed').textContent = m.tls_handshake_resumed_total || 0;

  // Phase 2/3: Top Targets & Users
  renderTopTargets(m.top_targets);
  renderTopUsers(m.users);

  // Phase 3: Backpressure
  document.getElementById('acceptDrops').textContent = m.accept_drops_total || 0;
  document.getElementById('bpEvents').textContent = m.backpressure_events || 0;

  // Phase 3: Mux
  document.getElementById('muxActive').textContent = m.mux_streams_active || 0;
  document.getElementById('muxTotal').textContent = m.mux_streams_total || 0;
  document.getElementById('muxPerConnP50').textContent = (m.mux_streams_per_conn_p50 || 0).toFixed(1);
  document.getElementById('muxPerConnP95').textContent = (m.mux_streams_per_conn_p95 || 0).toFixed(1);
}

function updateTable(conns) {
  var body = document.getElementById('connBody');
  var html = '';
  for (var i = 0; i < conns.length; i++) {
    var c = conns[i];
    var badge = c.status === 'active' ? 'badge-active' : 'badge-closed';
    html += '<tr>'
      + '<td>' + c.id + '</td>'
      + '<td>' + c.target + '</td>'
      + '<td class="up-color">' + fmtSpeed(c.upload_speed) + '</td>'
      + '<td class="down-color">' + fmtSpeed(c.download_speed) + '</td>'
      + '<td>' + fmtBytes(c.upload_bytes) + '</td>'
      + '<td>' + fmtBytes(c.download_bytes) + '</td>'
      + '<td>' + fmtDuration(c.duration) + '</td>'
      + '<td><span class="badge ' + badge + '">' + c.status + '</span></td>'
      + '</tr>';
  }
  body.innerHTML = html || '<tr><td colspan="8" style="text-align:center;color:#64748b;padding:32px">No connections</td></tr>';
}

function updateCluster(data) {
  var sec = document.getElementById('clusterSection');
  if (!data || !data.enabled) { sec.style.display = 'none'; return; }
  sec.style.display = 'block';

  // Mode badge
  var badge = document.getElementById('clModeBadge');
  if (data.force_relay) {
    badge.style.display = 'inline-block';
    badge.style.background = '#7c3aed';
    badge.style.color = '#f5f3ff';
    badge.textContent = 'FORCE RELAY';
  } else {
    badge.style.display = 'inline-block';
    badge.style.background = '#065f46';
    badge.style.color = '#6ee7b7';
    badge.textContent = 'OPTIMIZED';
  }

  document.getElementById('clLocalNode').textContent = data.local_node || '-';
  document.getElementById('clProbeInterval').textContent = data.probe_interval_sec || '-';
  var st = data.stats || {};
  document.getElementById('clTotalRelays').textContent = st.total_relays || 0;
  document.getElementById('clTotalFallbacks').textContent = st.total_fallbacks || 0;
  document.getElementById('clLastProbe').textContent = st.last_probe_at || '-';
  document.getElementById('clProbeTargets').textContent = st.probe_targets || 0;
  document.getElementById('clStaticTargets').textContent = st.static_targets || 0;
  document.getElementById('clDynamicTargets').textContent = st.dynamic_targets || 0;

  var peers = data.peers || [];

  // In force_relay mode, show total active relays instead of avg gain
  if (data.force_relay) {
    var totalActive = 0;
    for (var i = 0; i < peers.length; i++) totalActive += (peers[i].active_relays || 0);
    document.getElementById('clGainLabel').textContent = 'Active Relays';
    document.getElementById('clAvgGain').innerHTML = totalActive + '<span class="unit">conns</span>';
  } else {
    document.getElementById('clGainLabel').textContent = 'Avg Gain';
    document.getElementById('clAvgGain').innerHTML = (st.avg_gain_ms || 0).toFixed(1) + '<span class="unit">ms</span>';
  }

  // Peer table — enhanced for force_relay
  var phtml = '';
  for (var i = 0; i < peers.length; i++) {
    var p = peers[i];
    var badge2 = p.available ? 'badge-active' : 'badge-closed';
    var label = p.available ? 'online' : 'offline';
    var latency = (p.conn_latency_ms >= 0) ? (p.conn_latency_ms.toFixed(1) + ' ms') : '-';
    phtml += '<tr>'
      + '<td>' + p.name + '</td>'
      + '<td>' + p.host + ':' + p.port + '</td>'
      + '<td><span class="badge ' + badge2 + '">' + label + '</span></td>'
      + '<td>' + latency + '</td>'
      + '<td style="font-weight:600">' + (p.active_relays || 0) + '</td>'
      + '<td>' + (p.total_relays || 0) + '</td>'
      + '<td class="' + ((p.total_fallbacks || 0) > 0 ? 'err' : '') + '">' + (p.total_fallbacks || 0) + '</td>'
      + '</tr>';
  }
  document.getElementById('clPeersBody').innerHTML = phtml || '<tr><td colspan="7" style="text-align:center;color:#64748b">no peers</td></tr>';

  // Optimized Routes — hide in force_relay mode
  var routesCard = document.getElementById('clRoutesCard');
  if (data.force_relay) {
    routesCard.style.display = 'none';
    document.getElementById('clPeersCard').style.gridColumn = 'span 2';
  } else {
    routesCard.style.display = '';
    document.getElementById('clPeersCard').style.gridColumn = '';
    var routes = data.optimized_routes || [];
    var rhtml = '';
    for (var i = 0; i < routes.length; i++) {
      var rt = routes[i];
      var localCell = (rt.local_rtt_ms < 0) ? '<td style="color:#ef4444">Unreachable</td>' : '<td>' + rt.local_rtt_ms.toFixed(1) + ' ms</td>';
      var gainCell = (rt.local_rtt_ms < 0) ? '<td style="color:#f59e0b">Relay (blocked)</td>' : '<td style="color:#22c55e">-' + (rt.gain_ms || 0).toFixed(0) + ' ms (' + (rt.gain_percent || 0).toFixed(0) + '%)</td>';
      rhtml += '<tr><td style="font-family:ui-monospace,monospace">' + rt.target + '</td>' + localCell + '<td style="color:#22c55e">' + rt.best_peer + '</td><td>' + (rt.peer_rtt_ms || 0).toFixed(1) + ' ms</td>' + gainCell + '</tr>';
    }
    document.getElementById('clRoutesBody').innerHTML = rhtml || '<tr><td colspan="5" style="text-align:center;color:#64748b">no optimized routes</td></tr>';
  }
}

function fetchAll() {
  var hdrs = authHeaders();
  function getJSON(p) {
    return fetch(p, { headers: hdrs }).then(function(r) {
      if (r.status === 401) { doLogout(); throw 'unauthorized'; }
      return r.json();
    });
  }
  Promise.all([
    getJSON('/api/history'),
    getJSON('/api/summary'),
    getJSON('/api/connections'),
    getJSON('/api/metrics'),
    getJSON('/api/cluster')
  ]).then(function(results) {
    updateChart(results[0] || []);
    updateSummary(results[1]);
    updateTable(results[2] || []);
    updateMetrics(results[3]);
    updateCluster(results[4]);
  }).catch(function(e) { if (e !== 'unauthorized') console.error('fetch error:', e); });
}
</script>
</body>
</html>`
