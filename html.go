package main

import (
	"encoding/json"
	"strings"
)

// renderHTML embeds the run into a self-contained page; the charts are drawn
// client side from the same data run.json holds, so the page needs nothing
// from the network.
func renderHTML(r *Run) string {
	data, _ := json.Marshal(r)
	// A "</script>" inside a string would end the script element early.
	safe := strings.ReplaceAll(string(data), "</", "<\\/")
	page := strings.Replace(reportPage, "__TITLE__", r.Scenario+" ramp", 1)
	return strings.Replace(page, "__RUN__", safe, 1)
}

const reportPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>__TITLE__</title>
<style>
:root { --bg:#ffffff; --ink:#222222; --muted:#666666; --line:#cccccc; --grid:#e8e8e8; --head:#f2f2f2; --pass:#1a7f37; --fail:#c62828; }
@media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) { --bg:#1e1e1e; --ink:#e0e0e0; --muted:#9a9a9a; --line:#444444; --grid:#333333; --head:#2a2a2a; --pass:#4caf50; --fail:#ef5350; } }
:root[data-theme="dark"] { --bg:#1e1e1e; --ink:#e0e0e0; --muted:#9a9a9a; --line:#444444; --grid:#333333; --head:#2a2a2a; --pass:#4caf50; --fail:#ef5350; }
* { box-sizing:border-box; }
body { margin:0; padding-block:1.5rem 3rem; padding-inline:1.25rem; font:14px/1.5 Arial, Helvetica, sans-serif; color:var(--ink); background:var(--bg); }
main { max-width:1180px; margin-inline:auto; }
h1 { font-size:1.5rem; margin:0 0 .25rem; } h2 { font-size:1.1rem; margin:2rem 0 .5rem; } h3 { font-size:.95rem; margin:0 0 .3rem; }
p { margin:.25rem 0; } .muted { color:var(--muted); }
.meta { margin:.75rem 0 0; padding:0; list-style:none; font-size:.9rem; } .meta li { margin:.1rem 0; } .meta b { font-weight:normal; color:var(--muted); display:inline-block; min-width:9em; }
.verdict { margin:1.25rem 0; font-size:1rem; } .verdict ul { margin:.4rem 0 0; padding-left:1.4rem; }
.wrap { overflow-x:auto; }
table { border-collapse:collapse; font-size:.85rem; font-variant-numeric:tabular-nums; }
th, td { padding:.3rem .55rem; border:1px solid var(--line); text-align:right; white-space:nowrap; }
th:first-child, td:first-child { text-align:left; } th { background:var(--head); font-weight:bold; }
.summary td:first-child { color:var(--muted); }
.pass { color:var(--pass); font-weight:bold; } .fail { color:var(--fail); font-weight:bold; }
.grid { display:grid; grid-template-columns:repeat(auto-fit, minmax(400px, 1fr)); gap:1.5rem 2rem; }
svg { width:100%; height:auto; display:block; max-width:100%; }
.axis { font-size:10px; fill:var(--muted); font-family:Arial, Helvetica, sans-serif; } .gridline { stroke:var(--grid); } .marker { stroke:var(--muted); stroke-dasharray:3 3; } .marker.fail { stroke:var(--fail); }
.legend { font-size:.8rem; color:var(--muted); margin-top:.2rem; } .legend span { margin-right:1rem; }
.legend i { display:inline-block; width:12px; height:3px; margin-right:.35rem; vertical-align:middle; }
ul.diag { padding-left:1.4rem; font-size:.9rem; } code { font-family:Consolas, Menlo, monospace; font-size:.85em; }
@media (max-width: 500px) { .grid { grid-template-columns:1fr; } body { padding-inline:1rem; } }
</style>
</head>
<body>
<main>
<h1 id="title"></h1>
<p class="muted" id="desc"></p>
<ul class="meta" id="meta"></ul>
<div id="verdict" class="verdict"></div>
<h2>Summary</h2>
<div class="wrap"><table id="summary" class="summary"></table></div>
<h2>Steps</h2>
<div class="wrap"><table id="steps"></table></div>
<h2>Over time</h2>
<p class="muted">Dashed lines mark the start of each step, labelled with its target; a red one is the step that failed. Time is seconds since the run started.</p>
<div class="grid" id="charts"></div>
<h2 id="mounts-title">Per mount</h2>
<div class="wrap"><table id="mounts"></table></div>
<div id="extra"></div>
</main>
<script>
const RUN = __RUN__;
const PALETTE = ["#1f77b4","#d62728","#2ca02c","#ff7f0e","#9467bd","#17becf","#e377c2","#7f7f7f"];
const t0 = Date.parse(RUN.started);
const secs = iso => (Date.parse(iso) - t0) / 1000;
const fmt = (v, d = 1) => v == null ? "-" : Number(v).toFixed(d);
const esc = s => String(s).replace(/[&<>]/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;"}[c]));
const steps = RUN.steps || [], samples = RUN.samples || [], ticks = RUN.ticks || [];
const dur = s => s / 1e9 >= 60 ? (s / 6e10).toFixed(s % 6e10 ? 1 : 0) + " min" : s / 1e9 + " s";

document.getElementById("title").textContent = RUN.scenario;
document.getElementById("desc").textContent = RUN.description || "";
const c = RUN.config, h = RUN.host;
document.getElementById("meta").innerHTML = [
  ["started", RUN.started.replace("T", " ").slice(0, 19)],
  ["duration", Math.round((Date.parse(RUN.ended) - t0) / 1000) + " s"],
  ["host", h.cpus + " cpus, " + h.mem_mb + " MB, " + h.kernel + " " + h.arch],
  h.liquidsoap ? ["liquidsoap", h.liquidsoap] : null,
  c.remote ? ["remote server", c.remote + (RUN.server_host ? ", " + RUN.server_host.cpus + " cpus, " + RUN.server_host.mem_mb + " MB, " + RUN.server_host.kernel + " " + RUN.server_host.arch + (RUN.server_host.liquidsoap ? ", " + RUN.server_host.liquidsoap : "") : ", not sampled")] : null,
  ["ramp", c.start + " +" + c.step + " up to " + c.max + ", hold " + dur(c.hold)],
  ["listeners", "churn " + (c.Listener.Churn ? dur(c.Listener.Churn) : "off") + ", icy " + Math.round(c.Listener.ICY * 100) + "%, stall " + dur(c.Listener.Stall) + ", fail threshold " + (c.fail_threshold * 100).toFixed(1) + "%"],
].filter(Boolean).map(([k, v]) => "<li><b>" + esc(k) + "</b>" + esc(v) + "</li>").join("");

const v = document.getElementById("verdict");
const last = steps[steps.length - 1], firstFail = steps.find(s => !s.passed);
if (!steps.length || RUN.ceiling === 0) v.innerHTML = "<b>No step passed.</b>";
else if (RUN.max_reached) v.innerHTML = "<b>Every step passed up to the configured maximum of " + RUN.ceiling + " listeners.</b> Raise --max to find the ceiling.";
else v.innerHTML = "<b>Ceiling: " + RUN.ceiling + " listeners.</b> The next step failed.";
if (firstFail) v.innerHTML += "<p>First failing step, " + firstFail.target + " listeners:</p><ul>" + firstFail.reasons.map(r => "<li>" + esc(r) + "</li>").join("") + "</ul>";

const procs = [...new Set(samples.map(s => s.process))].sort();
const best = steps.filter(s => s.passed).pop() || firstFail || last;
const server = procs.find(p => p !== "icetest") || procs[0];
if (best) {
  const sp = (best.processes || {})[server] || {};
  const rows = [
    ["step", best.passed ? "best passing step, " + best.peak_live + " listeners" : "failing step, " + best.peak_live + " listeners"],
    ["throughput", fmt(best.avg_mbps) + " Mbit/s average, " + fmt(best.peak_mbps) + " peak"],
    server ? [server + " CPU", fmt(sp.cores_avg, 2) + " cores average, " + fmt(sp.cores_peak, 2) + " peak"] : null,
    server ? [server + " memory", fmt(sp.rss_peak_mb, 0) + " MB peak RSS"] : null,
    ["time to first byte", fmt(best.ttfb_p50_ms, 0) + " ms median, " + fmt(best.ttfb_p99_ms, 0) + " ms p99"],
  ].filter(Boolean);
  document.getElementById("summary").innerHTML = rows.map(([k, val]) => "<tr><td>" + esc(k) + "</td><td style='text-align:left'>" + esc(val) + "</td></tr>").join("");
}

const sumFails = s => Object.values(s.failures || {}).reduce((a, b) => a + b, 0);
const maxLag = s => Math.max(0, ...Object.values(s.mounts || {}).map(m => m.max_lagging));
let t = "<tr><th>listeners</th><th>ok</th><th>Mbit/s avg</th><th>Mbit/s peak</th><th>ttfb p50 / p99 ms</th><th>failures</th><th>lag max</th>" +
  procs.map(p => "<th>" + esc(p) + " cores</th><th>" + esc(p) + " MB</th><th>" + esc(p) + " fds</th>").join("") + "<th>sys cpu</th><th>canaries</th><th>alerts</th></tr>";
for (const s of steps) {
  t += "<tr><td>" + s.peak_live + "</td><td class='" + (s.passed ? "pass'>pass" : "fail'>FAIL") + "</td><td>" + fmt(s.avg_mbps) + "</td><td>" + fmt(s.peak_mbps) + "</td><td>" +
    fmt(s.ttfb_p50_ms, 0) + " / " + fmt(s.ttfb_p99_ms, 0) + "</td><td>" + sumFails(s) + "</td><td>" + maxLag(s) + "</td>" +
    procs.map(p => { const q = (s.processes || {})[p] || {}; return "<td>" + fmt(q.cores_avg, 2) + "</td><td>" + fmt(q.rss_peak_mb, 0) + "</td><td>" + (q.fds_peak ?? "-") + "</td>"; }).join("") +
    "<td>" + Math.round(s.system_cpu * 100) + "%</td><td>" + s.canary_ok + "/" + (s.canary_ok + s.canary_fail) + "</td><td>" + s.alerts + "</td></tr>";
}
document.getElementById("steps").innerHTML = t;

if (best) {
  document.getElementById("mounts-title").textContent = "Per mount at " + best.peak_live + " listeners";
  let m = "<tr><th>mount</th><th>live</th><th>connects</th><th>median kbps</th><th>min kbps</th><th>lag max</th><th>icy ok / bad</th><th>failures</th></tr>";
  for (const [name, q] of Object.entries(best.mounts).sort()) {
    const f = Object.entries(q.failures || {}).map(([k, n]) => k + "=" + n).join(" ") || "-";
    m += "<tr><td>" + esc(name) + "</td><td>" + q.live + "</td><td>" + q.connects + "</td><td>" + fmt(q.median_kbps, 0) + "</td><td>" + fmt(q.min_kbps, 0) + "</td><td>" + q.max_lagging + "</td><td>" + q.meta_ok + " / " + q.meta_bad + "</td><td>" + esc(f) + "</td></tr>";
  }
  document.getElementById("mounts").innerHTML = m;
}

// A series is {name, points: [[x, y]]}.
function bySecond(rows, key, reduce) {
  const acc = new Map();
  for (const r of rows) { const x = Math.round(secs(r.t)); acc.set(x, reduce(acc.get(x), r[key])); }
  return [...acc.entries()].sort((a, b) => a[0] - b[0]);
}
const sum = (a, b) => (a || 0) + b;
const byMount = (key, scale = 1) => [...new Set(ticks.map(t => t.mount))].sort().map(m => ({
  name: m, points: ticks.filter(t => t.mount === m).map(t => [secs(t.t), t[key] * scale]) }));
const byProc = key => procs.map(p => ({ name: p, points: samples.filter(s => s.process === p).map(s => [secs(s.t), s[key]]) }));
const markers = steps.map(s => ({ x: secs(s.start), label: String(s.target), fail: !s.passed }));

function lineChart(title, series, unit) {
  const W = 600, H = 250, L = 54, R = 14, T = 16, B = 30;
  const xs = series.flatMap(s => s.points.map(p => p[0])), ys = series.flatMap(s => s.points.map(p => p[1]));
  if (!xs.length) return;
  const xmin = Math.min(...xs), xmax = Math.max(...xs, xmin + 1);
  const ymax = Math.max(...ys) * 1.08 || 1;
  const sx = x => L + (x - xmin) / (xmax - xmin) * (W - L - R), sy = y => T + (1 - y / ymax) * (H - T - B);
  let g = "";
  for (const y of niceTicks(ymax, 5)) g += "<line class='gridline' x1='" + L + "' x2='" + (W - R) + "' y1='" + sy(y) + "' y2='" + sy(y) + "'/><text class='axis' x='" + (L - 6) + "' y='" + (sy(y) + 3.5) + "' text-anchor='end'>" + fmtTick(y) + "</text>";
  for (const x of niceTicks(xmax - xmin, 6)) g += "<text class='axis' x='" + sx(xmin + x) + "' y='" + (H - 8) + "' text-anchor='middle'>" + fmtTick(x) + "s</text>";
  for (const m of markers) { if (m.x < xmin || m.x > xmax) continue; const px = sx(m.x); g += "<line class='marker" + (m.fail ? " fail" : "") + "' x1='" + px + "' x2='" + px + "' y1='" + T + "' y2='" + (H - B) + "'/><text class='axis' x='" + (px + 3) + "' y='" + (T + 9) + "'>" + m.label + "</text>"; }
  series.forEach((s, i) => {
    if (!s.points.length) return;
    const color = PALETTE[i % PALETTE.length];
    const d = s.points.map((p, j) => (j ? "L" : "M") + sx(p[0]).toFixed(1) + " " + sy(p[1]).toFixed(1)).join("");
    g += "<path fill='none' stroke='" + color + "' stroke-width='1.6' stroke-linejoin='round' d='" + d + "'/>";
  });
  if (unit) g += "<text class='axis' x='" + (W - R) + "' y='" + (H - 8) + "' text-anchor='end'>" + esc(unit) + "</text>";
  const div = document.createElement("div");
  div.innerHTML = "<h3>" + esc(title) + "</h3><svg viewBox='0 0 " + W + " " + H + "' role='img' aria-label='" + esc(title) + "'>" + g + "</svg><div class='legend'>" +
    series.map((s, i) => "<span><i style='background:" + PALETTE[i % PALETTE.length] + "'></i>" + esc(s.name) + "</span>").join("") + "</div>";
  document.getElementById("charts").appendChild(div);
}
function niceTicks(max, n) { const raw = max / n, p = Math.pow(10, Math.floor(Math.log10(raw))), step = [1, 2, 5, 10].map(m => m * p).find(s => s >= raw); const out = []; for (let v = 0; v <= max; v += step) out.push(+v.toFixed(6)); return out; }
function fmtTick(v) { return v >= 1000 ? (v / 1000).toFixed(v % 1000 ? 1 : 0) + "k" : String(+v.toFixed(2)); }

lineChart("Listeners", [{ name: "live", points: bySecond(ticks, "live", sum) }], "");
lineChart("Throughput", [{ name: "Mbit/s", points: bySecond(ticks, "bps", sum).map(([x, y]) => [x, y * 8 / 1e6]) }], "Mbit/s");
lineChart("CPU", byProc("cores"), "cores");
lineChart("Memory", byProc("rss_mb"), "MB");
lineChart("File descriptors", byProc("fds"), "fds");
lineChart("Per-listener median rate", byMount("median_bps", 8 / 1000), "kbps");
lineChart("Lagging listeners", byMount("lagging"), "");
if (steps.length > 1) lineChart("Time to first byte per step", [
  { name: "p50", points: steps.map(s => [secs(s.start), s.ttfb_p50_ms]) }, { name: "p95", points: steps.map(s => [secs(s.start), s.ttfb_p95_ms]) },
  { name: "p99", points: steps.map(s => [secs(s.start), s.ttfb_p99_ms]) }], "ms");

let extra = "";
const failed = (RUN.canaries || []).filter(c => !c.ok);
if (failed.length) extra += "<h2>Failed canaries</h2><ul class='diag'>" + failed.map(c => "<li>step " + c.step + " " + esc(c.mount) + ": <code>" + esc((c.error || "").split("\n")[0]) + "</code></li>").join("") + "</ul>";
if ((RUN.alerts || []).length) extra += "<h2>Server log alerts</h2><ul class='diag'>" + RUN.alerts.slice(0, 20).map(a => "<li><code>" + esc(a.line) + "</code></li>").join("") + (RUN.alerts.length > 20 ? "<li>... " + (RUN.alerts.length - 20) + " more in run.json</li>" : "") + "</ul>";
if ((RUN.events || []).length) extra += "<h2>Listener events</h2><ul class='diag'>" + RUN.events.slice(0, 30).map(e => "<li><code>" + esc(e) + "</code></li>").join("") + (RUN.events.length > 30 ? "<li>... " + (RUN.events.length - 30) + " more in run.json</li>" : "") + "</ul>";
document.getElementById("extra").innerHTML = extra;
</script>
</body>
</html>
`
