package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// compareCmd puts several runs side by side, over the listener count rather
// than time, so different servers under the same ramp can be read together.
func compareCmd(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	out := fs.String("o", "", "directory to write compare.md and compare.html into")
	fs.Parse(args)
	if *out == "" || fs.NArg() < 2 {
		return fmt.Errorf("usage: icetest compare -o <out-dir> <run-dir> <run-dir>...")
	}
	var runs []*Run
	for _, dir := range fs.Args() {
		data, err := os.ReadFile(filepath.Join(dir, "run.json"))
		if err != nil {
			return err
		}
		var r Run
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		runs = append(runs, &r)
	}
	runs = mergeScenarios(runs)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	md := renderCompare(runs)
	if err := os.WriteFile(filepath.Join(*out, "compare.md"), []byte(md), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "compare.html"), []byte(renderCompareHTML(runs)), 0o644); err != nil {
		return err
	}
	fmt.Print(md)
	return nil
}

// mergeScenarios folds the runs of one scenario into a single series over the
// listener count. Where two runs stepped at the same target, the passing
// one wins, then the later one.
func mergeScenarios(runs []*Run) []*Run {
	var order []string
	byScenario := map[string]*Run{}
	for _, r := range runs {
		m, seen := byScenario[r.Scenario]
		if !seen {
			copy := *r
			copy.Steps = nil
			m = &copy
			byScenario[r.Scenario] = m
			order = append(order, r.Scenario)
		}
		for _, st := range r.Steps {
			replaced := false
			for i := range m.Steps {
				if m.Steps[i].Target == st.Target {
					if st.Passed || !m.Steps[i].Passed {
						m.Steps[i] = st
					}
					replaced = true
				}
			}
			if !replaced {
				m.Steps = append(m.Steps, st)
			}
		}
		m.Config.Start = min(m.Config.Start, r.Config.Start)
		m.Config.Max = max(m.Config.Max, r.Config.Max)
		if r.Started.Before(m.Started) {
			m.Started = r.Started
		}
		if r.Ended.After(m.Ended) {
			m.Ended = r.Ended
		}
	}
	var out []*Run
	for _, name := range order {
		m := byScenario[name]
		sort.Slice(m.Steps, func(i, j int) bool { return m.Steps[i].Target < m.Steps[j].Target })
		m.Ceiling, m.MaxReached = 0, true
		for _, st := range m.Steps {
			if st.Passed {
				m.Ceiling = max(m.Ceiling, st.Target)
			} else {
				m.MaxReached = false
			}
		}
		out = append(out, m)
	}
	return out
}

// serverProcess is the process holding the listener sockets: the one with
// the most file descriptors, load generators excluded.
func serverProcess(r *Run) string {
	best, fds := "", -1
	for _, s := range r.Steps {
		for name, p := range s.Processes {
			if !isLoadGenerator(name) && p.FdsPeak > fds {
				best, fds = name, p.FdsPeak
			}
		}
	}
	return best
}

func minStart(runs []*Run) int {
	m := runs[0].Config.Start
	for _, r := range runs {
		m = min(m, r.Config.Start)
	}
	return m
}

func maxTarget(runs []*Run) int {
	m := 0
	for _, r := range runs {
		for _, s := range r.Steps {
			m = max(m, s.Target)
		}
	}
	return m
}

func canariesPerStep(r *Run) int {
	if len(r.Steps) == 0 {
		return 0
	}
	return r.Steps[0].CanaryOK + r.Steps[0].CanaryFail
}

func runLabel(r *Run) string {
	if p := serverProcess(r); p != "" {
		return r.Scenario + " (" + p + ")"
	}
	return r.Scenario
}

func bestStep(r *Run) *StepResult {
	var best *StepResult
	for i := range r.Steps {
		if r.Steps[i].Passed {
			best = &r.Steps[i]
		}
	}
	return best
}

func renderCompare(runs []*Run) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	var labels []string
	for _, r := range runs {
		labels = append(labels, runLabel(r))
	}
	w("# %s\n\n", strings.Join(labels, " vs "))
	w("## Setup\n\n")
	w("### Servers under test\n\n")
	for _, r := range runs {
		w("- **%s**: %s\n", runLabel(r), strings.TrimSpace(r.Description))
	}
	w("\n### Machines\n\n")
	if h := runs[0].ServerHost; h != nil {
		w("- server: %d cpus, %d MB, %s %s", h.CPUs, h.MemMB, h.Kernel, h.Arch)
		if h.Liquidsoap != "" {
			w(", %s", h.Liquidsoap)
		}
		w("\n")
	}
	if l := loadSummary(runs[0]); l != "" {
		w("- %s; each load box: %d cpus, %d MB\n", l, runs[0].Host.CPUs, runs[0].Host.MemMB)
	}
	c := runs[0].Config
	w("\n### Listeners\n\n")
	w("- The listener count of a step is its level on the charts, from %d to %d here, spread evenly over the load boxes.\n", minStart(runs), maxTarget(runs))
	w("- A listener is a TCP connection that sends the HTTP GET a player sends and reads the stream for the whole step.\n")
	w("- It counts the bytes it receives. That is the throughput figure. It does not decode audio.\n")
	w("- %.0f%% of listeners request ICY metadata and parse every interleaved block.\n", c.Listener.ICY*100)
	w("- A listener fails when it receives nothing for %s, when the server disconnects it, or when its rate over the last %d s lags the median of its mount by more than %.0f%% for %d s in a row.\n", c.Listener.Stall, c.Listener.Window, c.Listener.LagTolerance*100, c.Listener.LagTicks)
	for _, r := range runs {
		w("- Admission against %s: up to %d new connections per second over the fleet, %s to connect and read the headers.\n", serverProcess(r), r.Config.Listener.ConnectRate, r.Config.Listener.ConnectTimeout)
	}
	w("\n### Canaries\n\n")
	w("- Decoding is checked by one ffmpeg process per load box, mount and step, %d per step here. Each joins the stream mid-way and must decode %s of it without error, as a player joining a running stream would.\n", canariesPerStep(runs[0]), c.CanaryDuration)
	w("\n### Pass rules\n\n")
	w("- Each level is held for %s once reached.\n", c.Hold)
	w("- A step fails above %.1f%% failed listeners, on a median rate under 90%% of nominal, on a canary that cannot decode, or on a server log alert.\n\n", c.FailThreshold*100)
	w("## Summary\n\n")
	w("| run | enabled | ceiling | Mbit/s at ceiling | server cores at ceiling | server RSS at ceiling | ttfb p50 / p99 at ceiling | range |\n|---|---|---|---|---|---|---|---|\n")
	for _, r := range runs {
		best := bestStep(r)
		verdict := "no step passed"
		cores, rss, ttfb := "-", "-", "-"
		if best != nil {
			verdict = fmt.Sprintf("%d", best.PeakLive)
			if r.MaxReached {
				verdict += " (maximum, not a ceiling)"
			}
			if p, ok := best.Processes[serverProcess(r)]; ok {
				cores = fmt.Sprintf("%.2f", p.CoresAvg)
				rss = fmt.Sprintf("%.0f MB", p.RSSPeakMB)
			}
			ttfb = fmt.Sprintf("%.0f / %.0f ms", best.TTFBp50ms, best.TTFBp99ms)
		}
		// The step average, not the instantaneous peak: peaks of several load
		// boxes summed do not happen in the same second.
		var peak float64
		for _, s := range r.Steps {
			if s.Passed {
				peak = max(peak, s.AvgMbps)
			}
		}
		last := 0
		if n := len(r.Steps); n > 0 {
			last = r.Steps[n-1].Target
		}
		w("| %s | %s | %s | %.0f | %s | %s | %s | %d to %d, hold %s |\n", runLabel(r), strings.Join(r.Config.With, ","), verdict, peak, cores, rss, ttfb, r.Config.Start, last, r.Config.Hold)
	}
	w("\n## Steps\n\n| listeners |")
	for _, l := range labels {
		w(" %s: ok | Mbit/s | ttfb p99 ms | server cores | server MB | failures |", l)
	}
	w("\n|---|")
	for range labels {
		w("---|---|---|---|---|---|")
	}
	w("\n")
	targets := map[int]bool{}
	for _, r := range runs {
		for _, s := range r.Steps {
			targets[s.Target] = true
		}
	}
	var sorted []int
	for t := range targets {
		sorted = append(sorted, t)
	}
	sort.Ints(sorted)
	for _, t := range sorted {
		w("| %d |", t)
		for _, r := range runs {
			var st *StepResult
			for i := range r.Steps {
				if r.Steps[i].Target == t {
					st = &r.Steps[i]
				}
			}
			if st == nil {
				w(" | | | | | |")
				continue
			}
			ok := "pass"
			if !st.Passed {
				ok = "FAIL"
			}
			var fails int64
			for _, n := range st.Failures {
				fails += n
			}
			p := st.Processes[serverProcess(r)]
			w(" %s | %.0f | %.0f | %.2f | %.0f | %d |", ok, st.AvgMbps, st.TTFBp99ms, p.CoresAvg, p.RSSPeakMB, fails)
		}
		w("\n")
	}
	return b.String()
}

func renderCompareHTML(runs []*Run) string {
	type series struct {
		Label       string       `json:"label"`
		Server      string       `json:"server"`
		Steps       []StepResult `json:"steps"`
		With        []string     `json:"with"`
		Config      Config       `json:"config"`
		Description string       `json:"description"`
		Host        HostInfo     `json:"host"`
		ServerHost  *HostInfo    `json:"server_host"`
		LoadBoxes   string       `json:"load_boxes"`
	}
	var all []series
	for _, r := range runs {
		all = append(all, series{Label: runLabel(r), Server: serverProcess(r), Steps: r.Steps, With: r.Config.With, Config: r.Config,
			Description: strings.TrimSpace(r.Description), Host: r.Host, ServerHost: r.ServerHost, LoadBoxes: loadSummary(r)})
	}
	data, _ := json.Marshal(all)
	safe := strings.ReplaceAll(string(data), "</", "<\\/")
	var labels []string
	for _, r := range runs {
		labels = append(labels, r.Scenario)
	}
	page := strings.Replace(comparePage, "__TITLE__", strings.Join(labels, " vs "), 1)
	return strings.Replace(page, "__RUNS__", safe, 1)
}

const comparePage = `<!doctype html>
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
.wrap { overflow-x:auto; }
table { border-collapse:collapse; font-size:.85rem; font-variant-numeric:tabular-nums; }
th, td { padding:.3rem .55rem; border:1px solid var(--line); text-align:right; white-space:nowrap; }
th:first-child, td:first-child { text-align:left; } th { background:var(--head); font-weight:bold; }
.pass { color:var(--pass); font-weight:bold; } .fail { color:var(--fail); font-weight:bold; }
.grid { display:grid; grid-template-columns:repeat(auto-fit, minmax(400px, 1fr)); gap:1.5rem 2rem; }
svg { width:100%; height:auto; display:block; max-width:100%; }
.axis { font-size:10px; fill:var(--muted); font-family:Arial, Helvetica, sans-serif; } .gridline { stroke:var(--grid); }
.legend { font-size:.8rem; color:var(--muted); margin-top:.2rem; } .legend span { margin-right:1rem; }
.legend i { display:inline-block; width:12px; height:3px; margin-right:.35rem; vertical-align:middle; }
.setup { display:grid; grid-template-columns:repeat(auto-fit, minmax(320px, 1fr)); gap:.5rem 2.5rem; max-width:1000px; }
.setup h3 { font-size:.95rem; margin:.75rem 0 .35rem; } .setup ul { margin:0; padding-left:1.2rem; } .setup li { margin:.25rem 0; max-width:60ch; }
#diagram svg { max-width:900px; margin:.5rem 0 1rem; } .box { fill:var(--head); stroke:var(--line); } .box.server { fill:var(--bg); stroke:var(--ink); } .lbl { font-size:11px; fill:var(--ink); font-family:Arial, Helvetica, sans-serif; } .lbl.muted { fill:var(--muted); } .arrow { stroke:var(--muted); fill:none; marker-end:url(#head); }
@media (max-width: 500px) { .grid { grid-template-columns:1fr; } body { padding-inline:1rem; } }
</style>
</head>
<body>
<main>
<h1 id="title"></h1>
<p class="muted">Same listeners, same pass rules, same server box. A cross marks a step that failed.</p>
<h2>Setup</h2>
<div id="diagram"></div>
<div id="setup" class="setup"></div>
<h2>Summary</h2>
<div class="wrap"><table id="summary"></table></div>
<h2>Over the listener count</h2>
<div class="grid" id="charts"></div>
<h2>Steps</h2>
<div class="wrap"><table id="steps"></table></div>
</main>
<script>
const RUNS = __RUNS__;
const PALETTE = ["#1f77b4","#d62728","#2ca02c","#ff7f0e","#9467bd","#17becf"];
const fmt = (v, d = 1) => v == null || isNaN(v) ? "-" : Number(v).toFixed(d);
const esc = s => String(s).replace(/[&<>]/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;"}[c]));
const dur = s => s / 1e9 >= 60 ? (s / 6e10).toFixed(s % 6e10 ? 1 : 0) + " min" : s / 1e9 + " s";
document.getElementById("title").textContent = RUNS.map(r => r.label).join(" vs ");
const server = (r, s) => (s.processes || {})[r.server] || {};
const best = r => r.steps.filter(s => s.passed).pop();
const fails = s => Object.values(s.failures || {}).reduce((a, b) => a + b, 0);

const c0 = RUNS[0].config, sh = RUNS[0].server_host, lh = RUNS[0].host;
// The fleet on the left, the server on the right with one lane per scenario.
(function () {
  const boxes = Math.max(1, ...RUNS.map(r => (r.load_boxes.match(/^load generators: (\d+)/) || [0, 1])[1] | 0));
  const lanes = RUNS.map(r => r.server === "icecast" ? ["liquidsoap: encode, output.icecast", "Icecast 2.4: listeners"] : ["liquidsoap: encode, output.harbor: listeners"]);
  const laneH = 26, serverH = 34 + lanes.reduce((n, l) => n + laneH, 0) + 30, loadH = 38, gap = 12;
  const boxW = p => 8 + p.length * 5.6;
  const laneW = l => l.reduce((n, p) => n + boxW(p) + 14, 0) - 14;
  const serverW = Math.max(430, 150 + Math.max(...lanes.map(laneW)) + 12);
  const loadX = serverW + 130, W = loadX + 200;
  const H = Math.max(serverH + 20, boxes * (loadH + gap) + 20);
  let g = "<defs><marker id='head' markerWidth='8' markerHeight='8' refX='7' refY='4' orient='auto'><path d='M0 0L8 4L0 8Z' fill='#666'/></marker></defs>";
  const cpuLoad = lh.cpus ? lh.cpus + " vCPU" : "", cpuServer = sh ? sh.cpus + " vCPU, " + Math.round(sh.mem_mb / 1024) + " GB" : "";
  g += "<rect class='box server' x='10' y='10' width='" + (serverW - 10) + "' height='" + serverH + "' rx='3'/>";
  g += "<text class='lbl' x='22' y='28'>server" + (cpuServer ? " (" + cpuServer + ")" : "") + "</text>";
  let y = 44;
  RUNS.forEach((r, i) => {
    const parts = lanes[i];
    g += "<text class='lbl' x='22' y='" + (y + 16) + "'>" + esc(r.server === "icecast" ? "icecast-reference" : r.label.replace(/ \(.*$/, "")) + "</text>";
    let x = 150;
    parts.forEach((p, j) => {
      const w = boxW(p);
      g += "<rect class='box' x='" + x + "' y='" + (y + 2) + "' width='" + w + "' height='" + (laneH - 6) + "' rx='2'/><text class='lbl' x='" + (x + 4) + "' y='" + (y + 15) + "'>" + esc(p) + "</text>";
      if (j < parts.length - 1) g += "<path class='arrow' d='M" + (x + w) + " " + (y + laneH / 2 - 1) + " L" + (x + w + 10) + " " + (y + laneH / 2 - 1) + "'/>";
      x += w + 14;
    });
    y += laneH;
  });
  g += "<text class='lbl muted' x='22' y='" + (y + 18) + "'>icetest serve: starts the processes, records CPU, memory, fds and log alerts once a second</text>";
  for (let i = 0; i < boxes; i++) {
    const by = 10 + i * (loadH + gap);
    const mid = serverW + 60;
    g += "<path class='arrow' d='M" + serverW + " " + (10 + serverH / 2) + " C" + mid + " " + (10 + serverH / 2) + " " + mid + " " + (by + loadH / 2) + " " + (loadX - 2) + " " + (by + loadH / 2) + "'/>";
    g += "<rect class='box' x='" + loadX + "' y='" + by + "' width='190' height='" + loadH + "' rx='3'/>";
    g += "<text class='lbl' x='" + (loadX + 10) + "' y='" + (by + 16) + "'>load box " + (i + 1) + (cpuLoad ? " (" + cpuLoad + ")" : "") + "</text>";
    g += "<text class='lbl muted' x='" + (loadX + 10) + "' y='" + (by + 30) + "'>icetest run: listeners, ffmpeg canary</text>";
  }
  g += "<text class='lbl muted' x='" + (serverW + 8) + "' y='" + (10 + serverH / 2 - 8) + "'>stream, internal network</text>";
  document.getElementById("diagram").innerHTML = "<svg viewBox='0 0 " + W + " " + H + "' role='img' aria-label='test architecture'>" + g + "</svg>";
})();
const section = (title, items) => "<div><h3>" + title + "</h3><ul>" + items.map(i => "<li>" + i + "</li>").join("") + "</ul></div>";
document.getElementById("setup").innerHTML =
  section("Servers under test", RUNS.map(r => "<b>" + esc(r.label) + "</b>: " + esc(r.description))) +
  section("Machines", [
    sh ? "server: " + sh.cpus + " cpus, " + sh.mem_mb + " MB, " + esc(sh.kernel + " " + sh.arch) + (sh.liquidsoap ? ", " + esc(sh.liquidsoap) : "") : null,
    RUNS[0].load_boxes ? esc(RUNS[0].load_boxes) + "; each load box: " + lh.cpus + " cpus, " + lh.mem_mb + " MB" : null].filter(Boolean)) +
  section("Listeners", [
    "The listener count of a step is its level on the charts, from " + Math.min(...RUNS.map(r => r.config.start)) + " to " + Math.max(...RUNS.flatMap(r => r.steps.map(s => s.target))) + " here, spread evenly over the load boxes.",
    "A listener is a TCP connection that sends the HTTP GET a player sends and reads the stream for the whole step.",
    "It counts the bytes it receives. That is the throughput figure. It does not decode audio.",
    Math.round(c0.Listener.ICY * 100) + "% of listeners request ICY metadata and parse every interleaved block.",
    "A listener fails when it receives nothing for " + dur(c0.Listener.Stall) + ", when the server disconnects it, or when its rate over the last " + c0.Listener.Window + " s lags the median of its mount by more than " + Math.round(c0.Listener.LagTolerance * 100) + "% for " + c0.Listener.LagTicks + " s in a row.",
    ...RUNS.map(r => "Admission against " + esc(r.server) + ": up to " + r.config.Listener.ConnectRate + " new connections per second over the fleet, " + dur(r.config.Listener.ConnectTimeout) + " to connect and read the headers.")]) +
  section("Canaries", ["Decoding is checked by one ffmpeg process per load box, mount and step, " + (RUNS[0].steps.length ? RUNS[0].steps[0].canary_ok + RUNS[0].steps[0].canary_fail : 0) + " per step here. Each joins the stream mid-way and must decode " + dur(c0.canary_duration) + " of it without error, as a player joining a running stream would."]) +
  section("Pass rules", [
    "Each level is held for " + dur(c0.hold) + " once reached.",
    "A step fails above " + (c0.fail_threshold * 100).toFixed(1) + "% failed listeners, on a median rate under 90% of nominal, on a canary that cannot decode, or on a server log alert."]);
let h = "<tr><th>run</th><th>enabled</th><th>ceiling</th><th>Mbit/s at ceiling</th><th>server cores at ceiling</th><th>server RSS at ceiling</th><th>ttfb p50 / p99 at ceiling</th><th>range</th></tr>";
for (const r of RUNS) {
  const b = best(r), c = r.config;
  const peak = Math.max(0, ...r.steps.filter(s => s.passed).map(s => s.avg_mbps));
  h += "<tr><td>" + esc(r.label) + "</td><td>" + esc((r.with || []).join(", ")) + "</td><td>" + (b ? b.peak_live + (r.steps.every(s => s.passed) ? " (maximum, not a ceiling)" : "") : "no step passed") +
    "</td><td>" + fmt(peak, 0) + "</td><td>" + (b ? fmt(server(r, b).cores_avg, 2) : "-") + "</td><td>" + (b ? fmt(server(r, b).rss_peak_mb, 0) + " MB" : "-") +
    "</td><td>" + (b ? fmt(b.ttfb_p50_ms, 0) + " / " + fmt(b.ttfb_p99_ms, 0) + " ms" : "-") + "</td><td>" + c.start + " to " + (r.steps.length ? r.steps[r.steps.length - 1].target : c.start) + ", hold " + dur(c.hold) + "</td></tr>";
}
document.getElementById("summary").innerHTML = h;

const targets = [...new Set(RUNS.flatMap(r => r.steps.map(s => s.target)))].sort((a, b) => a - b);
let t = "<tr><th>listeners</th>" + RUNS.map(r => "<th>" + esc(r.label) + "</th><th>Mbit/s</th><th>ttfb p99 ms</th><th>server cores</th><th>server MB</th><th>failures</th>").join("") + "</tr>";
for (const target of targets) {
  t += "<tr><td>" + target + "</td>";
  for (const r of RUNS) {
    const s = r.steps.find(s => s.target === target);
    if (!s) { t += "<td></td><td></td><td></td><td></td><td></td><td></td>"; continue; }
    const p = server(r, s);
    t += "<td class='" + (s.passed ? "pass'>pass" : "fail'>FAIL") + "</td><td>" + fmt(s.avg_mbps, 0) + "</td><td>" + fmt(s.ttfb_p99_ms, 0) + "</td><td>" + fmt(p.cores_avg, 2) + "</td><td>" + fmt(p.rss_peak_mb, 0) + "</td><td>" + fails(s) + "</td>";
  }
  t += "</tr>";
}
document.getElementById("steps").innerHTML = t;

function chart(title, unit, value) {
  const W = 600, H = 250, L = 54, R = 14, T = 16, B = 30;
  const series = RUNS.map(r => ({ name: r.label, points: r.steps.map(s => [s.peak_live, value(r, s), !s.passed]).filter(p => p[1] != null && !isNaN(p[1])) }));
  const xs = series.flatMap(s => s.points.map(p => p[0])), ys = series.flatMap(s => s.points.map(p => p[1]));
  if (!xs.length) return;
  const xmax = Math.max(...xs) * 1.05, ymax = Math.max(...ys) * 1.08 || 1;
  const sx = x => L + x / xmax * (W - L - R), sy = y => T + (1 - y / ymax) * (H - T - B);
  let g = "";
  for (const y of ticks(ymax, 5)) g += "<line class='gridline' x1='" + L + "' x2='" + (W - R) + "' y1='" + sy(y) + "' y2='" + sy(y) + "'/><text class='axis' x='" + (L - 6) + "' y='" + (sy(y) + 3.5) + "' text-anchor='end'>" + tick(y) + "</text>";
  for (const x of ticks(xmax, 6)) g += "<text class='axis' x='" + sx(x) + "' y='" + (H - 8) + "' text-anchor='middle'>" + tick(x) + "</text>";
  series.forEach((s, i) => {
    const color = PALETTE[i % PALETTE.length];
    g += "<path fill='none' stroke='" + color + "' stroke-width='1.6' stroke-linejoin='round' d='" + s.points.map((p, j) => (j ? "L" : "M") + sx(p[0]).toFixed(1) + " " + sy(p[1]).toFixed(1)).join("") + "'/>";
    for (const p of s.points) {
      const x = sx(p[0]).toFixed(1), y = sy(p[1]).toFixed(1);
      g += p[2] ? "<path stroke='" + color + "' stroke-width='1.6' d='M" + (x - 4) + " " + (y - 4) + "L" + (+x + 4) + " " + (+y + 4) + "M" + (x - 4) + " " + (+y + 4) + "L" + (+x + 4) + " " + (y - 4) + "'/>" : "<circle cx='" + x + "' cy='" + y + "' r='2.5' fill='" + color + "'/>";
    }
  });
  if (unit) g += "<text class='axis' x='" + (W - R) + "' y='" + (H - 8) + "' text-anchor='end'>" + esc(unit) + "</text>";
  const div = document.createElement("div");
  div.innerHTML = "<h3>" + esc(title) + "</h3><svg viewBox='0 0 " + W + " " + H + "' role='img' aria-label='" + esc(title) + "'>" + g + "</svg><div class='legend'>" +
    series.map((s, i) => "<span><i style='background:" + PALETTE[i % PALETTE.length] + "'></i>" + esc(s.name) + "</span>").join("") + "</div>";
  document.getElementById("charts").appendChild(div);
}
function ticks(max, n) { const raw = max / n, p = Math.pow(10, Math.floor(Math.log10(raw))), step = [1, 2, 5, 10].map(m => m * p).find(s => s >= raw); const out = []; for (let v = 0; v <= max; v += step) out.push(+v.toFixed(6)); return out; }
function tick(v) { return v >= 1000 ? (v / 1000).toFixed(v % 1000 ? 1 : 0) + "k" : String(+v.toFixed(2)); }

chart("Throughput", "Mbit/s", (r, s) => s.avg_mbps);
chart("Server CPU", "cores", (r, s) => server(r, s).cores_avg);
chart("Server memory", "MB", (r, s) => server(r, s).rss_peak_mb);
chart("Time to first byte, p99", "ms", (r, s) => s.ttfb_p99_ms);
chart("Time to first byte, median", "ms", (r, s) => s.ttfb_p50_ms);
chart("Failed listeners per step", "", (r, s) => fails(s));
</script>
</body>
</html>
`
