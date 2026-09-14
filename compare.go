package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// compareCmd puts several runs side by side, over the listener count rather
// than time, so different servers under the same ramp can be read together.
func compareCmd(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	out := fs.String("o", "", "directory to write compare.md and compare.html into")
	notesPath := fs.String("notes", "", "markdown file appended after the summary: the author's reading of the results")
	scenarios := fs.String("scenarios", "scenarios", "directory whose scenario.json files give the current description of each scenario")
	linkGbps := fs.Float64("link-gbps", 0, "egress cap of the server's NIC in Gbit/s, drawn against the NIC figure at the ceiling")
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
	docs := refreshDescriptions(runs, *scenarios)
	notes := ""
	if *notesPath != "" {
		data, err := os.ReadFile(*notesPath)
		if err != nil {
			return err
		}
		notes = string(data)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	md := renderCompare(runs, docs, notes)
	if err := os.WriteFile(filepath.Join(*out, "compare.md"), []byte(md), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "compare.html"), []byte(renderCompareHTML(runs, docs, notes, *linkGbps)), 0o644); err != nil {
		return err
	}
	fmt.Print(md)
	return nil
}

// mergeScenarios folds the runs of one scenario into a single series over the
// listener count, in the order given: a later ramp replaces the points of
// earlier ones inside the range it passed, and the series keeps the listener
// settings of the ramp that set its ceiling.
// passedRange is the span of listener counts a run held successfully.
func passedRange(r *Run) (lo, hi int, ok bool) {
	for _, st := range r.Steps {
		if st.Passed {
			if !ok || st.Target < lo {
				lo = st.Target
			}
			hi = max(hi, st.Target)
			ok = true
		}
	}
	return
}

func sameLevel(a, b int) bool {
	return max(a-b, b-a)*1000 <= max(a, b)
}

func mergeScenarios(runs []*Run) []*Run {
	var order []string
	byScenario := map[string]*Run{}
	ceilingOf := map[string]int{}
	for _, r := range runs {
		key := r.Scenario + " " + strings.Join(r.ServerEnv, " ")
		m, seen := byScenario[key]
		if !seen {
			copy := *r
			copy.Steps = nil
			m = &copy
			byScenario[key] = m
			order = append(order, key)
		}
		if lo, hi, ok := passedRange(r); ok {
			m.Steps = slices.DeleteFunc(m.Steps, func(st StepResult) bool { return st.Target >= lo && st.Target <= hi })
			if hi > ceilingOf[key] {
				ceilingOf[key] = hi
				m.Config.Listener = r.Config.Listener
				m.Config.Hold = r.Config.Hold
			}
		}
		for _, st := range r.Steps {
			replaced := false
			for i := range m.Steps {
				// Ramps with different step sizes land a few listeners apart
				// on the same level; that is one point, the later ramp's.
				if sameLevel(m.Steps[i].Target, st.Target) {
					if st.Passed || !m.Steps[i].Passed {
						m.Steps[i] = st
					}
					replaced = true
					break
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
		m.Ceiling = 0
		for _, st := range m.Steps {
			if st.Passed {
				m.Ceiling = max(m.Ceiling, st.Target)
			}
		}
		// A failure below a level another ramp passed was that ramp's surge,
		// not the server's ceiling; a step the fleet never reached is a
		// collapse with nothing measured, not a point on the curve.
		m.Steps = slices.DeleteFunc(m.Steps, func(st StepResult) bool {
			return !st.Passed && (st.Target < m.Ceiling || st.PeakLive < st.Target/2)
		})
		// A ramp that keeps stepping after its first failure measures a server
		// already down; one failure marks the ceiling.
		failedAt := 0
		m.Steps = slices.DeleteFunc(m.Steps, func(st StepResult) bool {
			if st.Passed {
				return false
			}
			if failedAt == 0 {
				failedAt = st.Target
			}
			return st.Target != failedAt
		})
		m.MaxReached = true
		for _, st := range m.Steps {
			if !st.Passed {
				m.MaxReached = false
			}
		}
		out = append(out, m)
	}
	return out
}

// serverProcess is the process holding the listener sockets: the one with
// the most file descriptors, load generators excluded.
// headline is what a reader takes away: how many listeners each server
// held, and what admission felt like for one of them at that point.
func headline(runs []*Run) [][4]string {
	ms := func(v float64) string {
		if v < 1000 {
			return fmt.Sprintf("%.0f ms", v)
		}
		return fmt.Sprintf("%.1f s", v/1000)
	}
	var rows [][4]string
	for _, r := range runs {
		if b := bestStep(r); b != nil {
			l := r.Config.Listener
			rows = append(rows, [4]string{runLabel(r), fmt.Sprintf("%dk", b.PeakLive/1000), fmt.Sprintf("%d/s, %s connect timeout", l.ConnectRate, l.ConnectTimeout), ms(b.TTFBp50ms) + " / " + ms(b.TTFBp99ms)})
		}
	}
	return rows
}

// refreshDescriptions takes each scenario's description from its current
// scenario.json, a run records the text of its day and the page should carry
// the text of today, and returns what the scenario says its settings mean.
func refreshDescriptions(runs []*Run, dir string) map[string]map[string]string {
	docs := map[string]map[string]string{}
	for _, r := range runs {
		data, err := os.ReadFile(filepath.Join(dir, r.Scenario, "scenario.json"))
		if err != nil {
			continue
		}
		var sc struct {
			Description string            `json:"description"`
			Settings    map[string]string `json:"settings"`
		}
		if json.Unmarshal(data, &sc) == nil && sc.Description != "" {
			r.Description = sc.Description
			docs[r.Scenario] = sc.Settings
		}
	}
	return docs
}

// A serverEntry is one scenario in "Servers under test": its series differ
// only by the settings named in their labels.
type serverEntry struct {
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Facts       string   `json:"facts"`
	Variants    []string `json:"variants"`
}

func serverEntries(runs []*Run, docs map[string]map[string]string) []serverEntry {
	var out []serverEntry
	index := map[string]int{}
	for _, r := range runs {
		key := r.Scenario + " " + serverProcess(r)
		i, seen := index[key]
		if !seen {
			label := r.Scenario
			if p := serverProcess(r); p != "" {
				label += " (" + p + ")"
			}
			facts := serverFacts(r)
			if len(r.ServerEnv) > 0 {
				facts = strings.Replace(facts, "settings "+strings.Join(r.ServerEnv, ", ")+"; ", "", 1)
			}
			out = append(out, serverEntry{Label: label, Description: strings.TrimSpace(r.Description), Facts: facts})
			i = len(out) - 1
			index[key] = i
		}
		if len(r.ServerEnv) == 0 {
			out[i].Variants = append(out[i].Variants, "default settings")
			continue
		}
		var parts []string
		for _, kv := range r.ServerEnv {
			name, _, _ := strings.Cut(kv, "=")
			if doc := docs[r.Scenario][name]; doc != "" {
				parts = append(parts, kv+", which "+doc)
			} else {
				parts = append(parts, kv)
			}
		}
		out[i].Variants = append(out[i].Variants, strings.Join(parts, "; "))
	}
	return out
}

// A loadSpan is a stretch of a series driven by the same number of load
// boxes; the fleet can grow between the ramps folded into one scenario.
type loadSpan struct {
	Boxes int `json:"boxes"`
	From  int `json:"from"`
}

func loadSpans(r *Run) []loadSpan {
	var out []loadSpan
	for _, s := range r.Steps {
		n := 0
		for name := range s.Processes {
			if isLoadGenerator(name) {
				n++
			}
		}
		if len(out) == 0 || out[len(out)-1].Boxes != n {
			out = append(out, loadSpan{n, s.Target})
		}
	}
	return out
}

// loadBoxesText says how many load boxes there were, and where more joined.
func loadBoxesText(runs []*Run) string {
	lo, hi := 0, 0
	var more []string
	for _, r := range runs {
		for i, sp := range loadSpans(r) {
			if lo == 0 || sp.Boxes < lo {
				lo = sp.Boxes
			}
			hi = max(hi, sp.Boxes)
			if i > 0 {
				more = append(more, fmt.Sprintf("%d for %s from %d listeners up", sp.Boxes, runLabel(r), sp.From))
			}
		}
	}
	if lo == 0 {
		return ""
	}
	if lo == hi {
		return fmt.Sprintf("%d load boxes", lo)
	}
	return fmt.Sprintf("%d load boxes, %s", lo, strings.Join(more, "; "))
}

// serverFacts is what ran, read from the run rather than written by hand:
// the build, the settings, the mounts and their rate.
func serverFacts(r *Run) string {
	var facts []string
	if r.ServerHost != nil && r.ServerHost.Liquidsoap != "" {
		facts = append(facts, r.ServerHost.Liquidsoap)
	}
	if len(r.ServerEnv) > 0 {
		facts = append(facts, "settings "+strings.Join(r.ServerEnv, ", "))
	}
	if b := bestStep(r); b != nil {
		var mounts []string
		for _, path := range slices.Sorted(maps.Keys(b.Mounts)) {
			mounts = append(mounts, fmt.Sprintf("%s at %.0f kbit/s", path, b.Mounts[path].MedianKbps))
		}
		facts = append(facts, strings.Join(mounts, ", "))
	}
	return strings.Join(facts, "; ")
}

// renderNotes turns the small markdown the notes use (headings, paragraphs,
// bullets, bold, code) into HTML.
func renderNotes(md string) string {
	if strings.TrimSpace(md) == "" {
		return ""
	}
	inline := func(s string) string {
		s = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
		for _, m := range [][2]string{{"**", "b"}, {"`", "code"}} {
			for {
				a := strings.Index(s, m[0])
				if a < 0 {
					break
				}
				b := strings.Index(s[a+len(m[0]):], m[0])
				if b < 0 {
					break
				}
				b += a + len(m[0])
				s = s[:a] + "<" + m[1] + ">" + s[a+len(m[0]):b] + "</" + m[1] + ">" + s[b+len(m[0]):]
			}
		}
		return s
	}
	var b strings.Builder
	var para []string
	inList := false
	flush := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + inline(strings.Join(para, " ")) + "</p>\n")
			para = nil
		}
		if inList {
			b.WriteString("</ul>\n")
			inList = false
		}
	}
	for _, line := range strings.Split(md, "\n") {
		switch {
		case strings.HasPrefix(line, "### "):
			flush()
			b.WriteString("<h3>" + inline(line[4:]) + "</h3>\n")
		case strings.HasPrefix(line, "## "):
			flush()
			b.WriteString("<h2>" + inline(line[3:]) + "</h2>\n")
		case strings.HasPrefix(line, "- "):
			if len(para) > 0 {
				flush()
			}
			if !inList {
				b.WriteString("<ul>\n")
				inList = true
			}
			b.WriteString("<li>" + inline(line[2:]) + "</li>\n")
		case strings.TrimSpace(line) == "":
			flush()
		default:
			if inList {
				flush()
			}
			para = append(para, strings.TrimSpace(line))
		}
	}
	flush()
	return b.String()
}

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

func rateOf(runs []*Run, server string) int {
	for _, r := range runs {
		if serverProcess(r) == server {
			return r.Config.Listener.ConnectRate
		}
	}
	return 0
}

func timeoutOf(runs []*Run, server string) time.Duration {
	for _, r := range runs {
		if serverProcess(r) == server {
			return r.Config.Listener.ConnectTimeout
		}
	}
	return 0
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

func runLabel(r *Run) string {
	label := r.Scenario
	if p := serverProcess(r); p != "" {
		label += " (" + p + ")"
	}
	if len(r.ServerEnv) > 0 {
		label += " " + strings.Join(r.ServerEnv, " ")
	}
	return label
}

// writeMachineTable is the server machine at each ceiling, when the runs
// carry a serve recording made with the machine sampler.
func writeMachineTable(w func(string, ...any), runs []*Run) {
	rows := false
	for _, r := range runs {
		if b := bestStep(r); b != nil && b.Server != nil {
			rows = true
		}
	}
	if !rows {
		return
	}
	w("\n## Server machine at the ceiling\n\n")
	w("| run | busy per core | user / system / interrupts | memory used | socket buffers | NIC out | packets out /s | retransmits /s |\n|---|---|---|---|---|---|---|---|\n")
	for _, r := range runs {
		b := bestStep(r)
		if b == nil || b.Server == nil {
			continue
		}
		m := b.Server
		cores := make([]string, len(m.Cores))
		for i, c := range m.Cores {
			cores[i] = fmt.Sprintf("%.0f%%", c*100)
		}
		w("| %s | %s | %.0f%% / %.0f%% / %.0f%% | %.0f MB | %.0f MB | %.0f Mbit/s | %.0f | %.0f |\n",
			runLabel(r), strings.Join(cores, " "), m.User*100, m.System*100, m.IRQ*100, m.MemUsedMB, m.SockMemMB, m.TxMbps, m.TxPps, m.Retrans)
	}
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

func renderCompare(runs []*Run, docs map[string]map[string]string, notes string) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	var labels []string
	for _, r := range runs {
		labels = append(labels, runLabel(r))
	}
	w("# %s\n\n", strings.Join(labels, " vs "))
	if rows := headline(runs); len(rows) > 0 {
		w("## Results\n\n| | listeners held | arrivals* | admission p50 / p99 |\n|---|---|---|---|\n")
		for _, row := range rows {
			w("| **%s** | **%s** | %s | %s |\n", row[0], row[1], row[2], row[3])
		}
		w("\n\\* The arrival rate and connect timeout, the deadline for the TCP connect and the HTTP response headers, are the test settings under which the ramp passed, not a measured limit: Icecast could not keep up with the 300 per second the liquidsoap ramps used.\n\n")
	}
	w("## Setup\n\n")
	w("### Servers under test\n\n")
	for _, e := range serverEntries(runs, docs) {
		w("- **%s**: %s (%s)", e.Label, e.Description, e.Facts)
		if len(e.Variants) > 1 {
			w(" Measured with %s.", strings.Join(e.Variants, ", and with "))
		}
		w("\n")
	}
	w("\n### Machines\n\n")
	if h := runs[0].ServerHost; h != nil {
		w("- server: %d cpus, %.0f GB, %s %s\n", h.CPUs, float64(h.MemMB)/1000, h.Kernel, h.Arch)
	}
	if l := loadBoxesText(runs); l != "" {
		w("- %s; %d cpus each\n", l, runs[0].Host.CPUs)
	}
	c := runs[0].Config
	w("\n### Scale\n\n")
	w("- Listeners per step: from %d to %d, the level on the charts, spread evenly over the load boxes; one canary per load box.\n", minStart(runs), maxTarget(runs))
	w("\n### Admission\n\n")
	w("- How fast new listeners arrive is part of the test and differs by server. **liquidsoap was measured under a surge of %d new connections per second with a %s connect timeout; Icecast needed the rate cut to %d per second and the timeout raised to %s to admit listeners at all.**\n",
		rateOf(runs, "liquidsoap"), timeoutOf(runs, "liquidsoap"), rateOf(runs, "icecast"), timeoutOf(runs, "icecast"))
	w("\n### Listeners\n\n")
	w("- A listener is a TCP connection that sends the HTTP GET a player sends, reads the stream for the whole step and counts the bytes: that is the throughput figure. It does not decode audio; %.0f%% of listeners request ICY metadata and parse every interleaved block.\n", c.Listener.ICY*100)
	w("- A listener fails when it receives nothing for %s, when the server disconnects it, or when its rate over the last %d s lags the median of its mount by more than %.0f%% for %d s in a row.\n", c.Listener.Stall, c.Listener.Window, c.Listener.LagTolerance*100, c.Listener.LagTicks)
	w("\n### Canaries\n\n")
	w("- Decoding is checked by an ffmpeg process per load box, mount and step. Each joins the stream mid-way and must decode %s of it without error, as a player joining a running stream would.\n", c.CanaryDuration)
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
	if strings.TrimSpace(notes) != "" {
		w("\n%s\n", strings.TrimSpace(notes))
	}
	writeMachineTable(w, runs)
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

func renderCompareHTML(runs []*Run, docs map[string]map[string]string, notes string, linkGbps float64) string {
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
		LoadSpans   []loadSpan   `json:"load_spans"`
		Facts       string       `json:"facts"`
	}
	var all []series
	for _, r := range runs {
		all = append(all, series{Label: runLabel(r), Server: serverProcess(r), Steps: r.Steps, With: r.Config.With, Config: r.Config,
			Description: strings.TrimSpace(r.Description), Host: r.Host, ServerHost: r.ServerHost, LoadBoxes: loadSummary(r), LoadSpans: loadSpans(r), Facts: serverFacts(r)})
	}
	data, _ := json.Marshal(all)
	safe := strings.ReplaceAll(string(data), "</", "<\\/")
	var labels []string
	for _, r := range runs {
		labels = append(labels, r.Scenario)
	}
	page := strings.Replace(comparePage, "__TITLE__", strings.Join(labels, " vs "), 1)
	page = strings.Replace(page, "__RUNS__", safe, 1)
	page = strings.Replace(page, "__LINK__", fmt.Sprintf("%g", linkGbps), 1)
	servers, _ := json.Marshal(serverEntries(runs, docs))
	page = strings.Replace(page, "__SERVERS__", strings.ReplaceAll(string(servers), "</", "<\\/"), 1)
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
	head := "<tr><th></th><th>listeners held</th><th>arrivals*</th><th>admission p50 / p99</th></tr>"
	for _, row := range headline(runs) {
		head += "<tr><td>" + esc(row[0]) + "</td><td>" + esc(row[1]) + "</td><td>" + esc(row[2]) + "</td><td>" + esc(row[3]) + "</td></tr>"
	}
	page = strings.Replace(page, "__HEADLINE__", head, 1)
	notesJSON, _ := json.Marshal(renderNotes(notes))
	return strings.Replace(page, "__NOTES__", strings.ReplaceAll(string(notesJSON), "</", "<\\/"), 1)
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
.cores { display:grid; grid-template-columns:repeat(auto-fit, minmax(260px, 1fr)); gap:1rem 2rem; max-width:1100px; } .cores h3 { font-size:.95rem; margin:.5rem 0 .25rem; }
.cores svg { max-width:340px; } .mem.free { fill:var(--head); stroke:var(--line); } .mem.rss { fill:var(--ink); } .mem.sock { fill:var(--ink); opacity:.55; } .mem.other { fill:var(--ink); opacity:.25; } .cores .bar { fill:var(--ink); opacity:.75; } .cores .bar.hot { opacity:1; } .cores p { margin:.25rem 0 0; font-size:.85rem; color:var(--muted); max-width:40ch; }
.setup { display:grid; grid-template-columns:repeat(auto-fit, minmax(320px, 1fr)); gap:.5rem 2.5rem; max-width:1000px; }
.toc { font-size:.9rem; margin:0 0 1.5rem; } .toc a { margin-right:1.2rem; white-space:nowrap; color:var(--ink); } h2 { scroll-margin-top:1rem; }
.headline { margin:0 0 .5rem; } .footnote { font-size:.85rem; max-width:70ch; margin:0 0 1.25rem; } .headline td:first-child, .headline td:nth-child(2) { font-weight:bold; } .headline td:nth-child(2) { font-size:1.2rem; }
.notes { max-width:70ch; } .notes h3 { font-size:1rem; margin:1rem 0 .3rem; } .notes p, .notes li { line-height:1.45; }
.setup h3 { font-size:.95rem; margin:.75rem 0 .35rem; } .setup ul { margin:0; padding-left:1.2rem; } .setup li { margin:.25rem 0; max-width:60ch; }
#diagram svg { max-width:900px; margin:.5rem 0 1rem; } .box { fill:var(--head); stroke:var(--line); } .box.server { fill:var(--bg); stroke:var(--ink); } .lbl { font-size:11px; fill:var(--ink); font-family:Arial, Helvetica, sans-serif; } .lbl.muted { fill:var(--muted); } .arrow { stroke:var(--muted); fill:none; marker-end:url(#head); }
@media (max-width: 500px) { .grid { grid-template-columns:1fr; } body { padding-inline:1rem; } }
</style>
</head>
<body>
<main>
<h1 id="title"></h1>
<p class="muted">Same listeners, same pass rules, same server box. A cross marks a step that failed.</p>
<nav class="toc" id="toc"></nav>
<div id="diagram"></div>
<h2>Results</h2>
<div class="wrap"><table class="headline">__HEADLINE__</table></div>
<p class="muted footnote">* The arrival rate and connect timeout, the deadline for the TCP connect and the HTTP response headers, are the test settings under which the ramp passed, not a measured limit: Icecast could not keep up with the 300 per second the liquidsoap ramps used.</p>
<h2>Setup</h2>
<div id="setup" class="setup"></div>
<h2>Summary</h2>
<div class="wrap"><table id="summary"></table></div>
<div id="notes" class="notes"></div>
<div id="machine"></div>
<h2>Over the listener count</h2>
<div class="grid" id="charts"></div>
<h2>Steps</h2>
<div class="wrap"><table id="steps"></table></div>
</main>
<script>
const RUNS = __RUNS__;
const NOTES = __NOTES__;
const SERVERS = __SERVERS__;
const LINK_GBPS = __LINK__;
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
  const spans = RUNS.flatMap(r => r.load_spans || []);
  const boxes = Math.max(1, ...spans.map(s => s.boxes)), always = Math.min(...spans.map(s => s.boxes));
  const joinedAt = Math.min(...spans.filter(s => s.boxes > always).map(s => s.from));
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
    g += "<text class='lbl muted' x='" + (loadX + 10) + "' y='" + (by + 30) + "'>" + (i >= always ? "joined from " + tick(joinedAt) + " listeners up" : "icetest run: listeners, ffmpeg canary") + "</text>";
  }
  g += "<text class='lbl muted' x='" + (serverW + 8) + "' y='" + (10 + serverH / 2 - 8) + "'>stream, internal network</text>";
  document.getElementById("diagram").innerHTML = "<svg viewBox='0 0 " + W + " " + H + "' role='img' aria-label='test architecture'>" + g + "</svg>";
})();
const section = (title, items) => "<div><h3>" + title + "</h3><ul>" + items.map(i => "<li>" + i + "</li>").join("") + "</ul></div>";
function loadBoxesText() {
  const spans = RUNS.flatMap(r => (r.load_spans || []).map((s, i) => ({ ...s, run: r.label, first: i === 0 })));
  if (!spans.length) return "";
  const lo = Math.min(...spans.map(s => s.boxes)), hi = Math.max(...spans.map(s => s.boxes));
  if (lo === hi) return lo + " load boxes";
  return lo + " load boxes, " + spans.filter(s => !s.first).map(s => s.boxes + " for " + esc(s.run) + " from " + s.from + " listeners up").join("; ");
}
document.getElementById("notes").innerHTML = NOTES;
document.getElementById("setup").innerHTML =
  section("Servers under test", SERVERS.map(e => "<b>" + esc(e.label) + "</b>: " + esc(e.description) + (e.facts ? " <span class='muted'>(" + esc(e.facts) + ")</span>" : "") + (e.variants.length > 1 ? " Measured with " + e.variants.map(esc).join(", and with ") + "." : ""))) +
  section("Machines", [
    sh ? "server: " + sh.cpus + " cpus, " + Math.round(sh.mem_mb / 1000) + " GB, " + esc(sh.kernel + " " + sh.arch) : null,
    loadBoxesText() ? loadBoxesText() + "; " + lh.cpus + " cpus each" : null].filter(Boolean)) +
  section("Scale", [
    "Listeners per step: from " + Math.min(...RUNS.map(r => r.config.start)) + " to " + Math.max(...RUNS.flatMap(r => r.steps.map(s => s.target))) + ", the level on the charts, spread evenly over the load boxes; one canary per load box."]) +
  section("Admission", [(function () {
    const by = name => RUNS.find(r => r.server === name);
    const l = by("liquidsoap"), i = by("icecast");
    if (!l || !i) return "How fast new listeners arrive is part of the test: " + RUNS.map(r => esc(r.server) + " at up to " + r.config.Listener.ConnectRate + " new connections per second, " + dur(r.config.Listener.ConnectTimeout) + " connect timeout").join("; ") + ".";
    return "How fast new listeners arrive is part of the test and differs by server. <b>liquidsoap was measured under a surge of " + l.config.Listener.ConnectRate + " new connections per second with a " + dur(l.config.Listener.ConnectTimeout) + " connect timeout; Icecast needed the rate cut to " + i.config.Listener.ConnectRate + " per second and the timeout raised to " + dur(i.config.Listener.ConnectTimeout) + " to admit listeners at all.</b>";
  })()]) +
  section("Listeners", [
    "A listener is a TCP connection that sends the HTTP GET a player sends, reads the stream for the whole step and counts the bytes: that is the throughput figure. It does not decode audio; " + Math.round(c0.Listener.ICY * 100) + "% of listeners request ICY metadata and parse every interleaved block.",
    "A listener fails when it receives nothing for " + dur(c0.Listener.Stall) + ", when the server disconnects it, or when its rate over the last " + c0.Listener.Window + " s lags the median of its mount by more than " + Math.round(c0.Listener.LagTolerance * 100) + "% for " + c0.Listener.LagTicks + " s in a row."]) +
  section("Canaries", ["Decoding is checked by an ffmpeg process per load box, mount and step. Each joins the stream mid-way and must decode " + dur(c0.canary_duration) + " of it without error, as a player joining a running stream would."]) +
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

// One figure per run: a bar per core at the ceiling step, so an uneven load
// (one saturated thread on an idle box) is visible next to a flat one.
const machineRuns = RUNS.map(r => [r, best(r)]).filter(([r, b]) => b && b.server && b.server.cores && b.server.cores.length);
if (machineRuns.length) {
  let m = "<h2>Server machine at the ceiling</h2><p class='muted'>Busy time per core, averaged over the ceiling step. Below each: where the box's time went, memory, and the NIC.</p><div class='cores'>";
  for (const [r, b] of machineRuns) {
    const s = b.server, n = s.cores.length, W = 340, H = 120, L = 30, B = 18, bw = (W - L - 8) / n;
    let g = "";
    for (const y of [0, 0.5, 1]) g += "<line class='gridline' x1='" + L + "' x2='" + (W - 8) + "' y1='" + (H - B - y * (H - B - 6)) + "' y2='" + (H - B - y * (H - B - 6)) + "'/><text class='axis' x='" + (L - 4) + "' y='" + (H - B - y * (H - B - 6) + 3.5) + "' text-anchor='end'>" + (y * 100) + "%</text>";
    s.cores.forEach((c, i) => {
      const hgt = c * (H - B - 6);
      g += "<rect class='bar" + (c > 0.9 ? " hot" : "") + "' x='" + (L + i * bw + 2) + "' y='" + (H - B - hgt) + "' width='" + (bw - 4) + "' height='" + hgt + "'/>";
      g += "<text class='axis' x='" + (L + i * bw + bw / 2) + "' y='" + (H - 5) + "' text-anchor='middle'>" + i + "</text>";
    });
    // Memory as one bar of the box's RAM: the server process, the kernel's
    // socket buffers, everything else in use, and what is free.
    const total = (r.server_host || {}).mem_mb || 0, rss = server(r, b).rss_peak_mb || 0, sock = s.sock_mem_mb || 0, other = Math.max(0, s.mem_used_mb - rss - sock);
    let mem = "";
    if (total > 0) {
      const MW = W - L - 8, MH = 16, mx = v => v / total * MW;
      let x = L;
      for (const [v, cls] of [[rss, "rss"], [sock, "sock"], [other, "other"]]) {
        if (v > 0) mem += "<rect class='mem " + cls + "' x='" + x + "' y='4' width='" + Math.max(mx(v), 1) + "' height='" + MH + "'/>";
        x += mx(v);
      }
      mem = "<svg viewBox='0 0 " + W + " 40' role='img' aria-label='memory'><rect class='mem free' x='" + L + "' y='4' width='" + MW + "' height='" + MH + "'/>" + mem +
        "<text class='axis' x='" + (L - 4) + "' y='15' text-anchor='end'>RAM</text>" +
        "<text class='axis' x='" + L + "' y='34'>" + fmt(s.mem_used_mb / 1000, 1) + " GB used of " + Math.round(total / 1000) + " GB: " + esc(r.server) + " " + fmt(rss, 0) + " MB, socket buffers " + fmt(sock, 0) + " MB</text></svg>";
    }
    // The NIC as one bar of the link, when the link's cap is known.
    let nic = "";
    if (LINK_GBPS > 0) {
      const MW = W - L - 8, MH = 16, used = Math.min(s.tx_mbps / 1000 / LINK_GBPS, 1);
      nic = "<svg viewBox='0 0 " + W + " 40' role='img' aria-label='NIC'><rect class='mem free' x='" + L + "' y='4' width='" + MW + "' height='" + MH + "'/><rect class='mem rss' x='" + L + "' y='4' width='" + Math.max(used * MW, 1) + "' height='" + MH + "'/>" +
        "<text class='axis' x='" + (L - 4) + "' y='15' text-anchor='end'>NIC</text>" +
        "<text class='axis' x='" + L + "' y='34'>" + fmt(s.tx_mbps / 1000, 1) + " of " + LINK_GBPS + " Gbit/s out, " + fmt(s.tx_pps / 1000, 0) + "k packets/s, " + fmt(s.retrans_per_s, 0) + " retransmits/s</text></svg>";
    }
    m += "<div><h3>" + esc(r.label) + " at " + b.peak_live + "</h3><svg viewBox='0 0 " + W + " " + H + "' role='img' aria-label='busy per core'>" + g + "</svg>" + mem + nic +
      "<p>" + esc(r.server) + " process " + fmt(server(r, b).cores_avg, 2) + " cores; user " + Math.round(s.user * 100) + "%, system " + Math.round(s.system * 100) + "%, interrupts " + Math.round(s.irq * 100) + "%" + (s.steal > 0.005 ? ", steal " + Math.round(s.steal * 100) + "%" : "") + " of all core time." +
      (LINK_GBPS > 0 ? "" : "<br>NIC out " + fmt(s.tx_mbps / 1000, 1) + " Gbit/s, " + fmt(s.tx_pps / 1000, 0) + "k packets/s, " + fmt(s.retrans_per_s, 0) + " retransmits/s.") + "</p></div>";
  }
  document.getElementById("machine").innerHTML = m + "</div>";
}

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

// limit is what the server box has of the plotted quantity: drawn as a
// dashed line when the series come near it, named in the legend otherwise.
function chart(title, unit, value, limit) {
  const W = 600, H = 250, L = 54, R = 14, T = 16, B = 30;
  const series = RUNS.map(r => ({ name: r.label, points: r.steps.map(s => [s.peak_live, value(r, s), !s.passed]).filter(p => p[1] != null && !isNaN(p[1])) }));
  const xs = series.flatMap(s => s.points.map(p => p[0])), ys = series.flatMap(s => s.points.map(p => p[1]));
  if (!xs.length) return;
  const drawLimit = limit && limit.value > 0 && Math.max(...ys) > limit.value / 4;
  const xmax = Math.max(...xs) * 1.05, ymax = Math.max(Math.max(...ys) * 1.08, drawLimit ? limit.value * 1.08 : 0) || 1;
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
  if (drawLimit) g += "<line x1='" + L + "' x2='" + (W - R) + "' y1='" + sy(limit.value) + "' y2='" + sy(limit.value) + "' stroke='var(--ink)' stroke-dasharray='5 4' stroke-width='1'/><text class='axis' x='" + (W - R) + "' y='" + (sy(limit.value) - 4) + "' text-anchor='end' fill='var(--ink)'>" + esc(limit.label) + "</text>";
  const div = document.createElement("div");
  div.innerHTML = "<h3>" + esc(title) + "</h3><svg viewBox='0 0 " + W + " " + H + "' role='img' aria-label='" + esc(title) + "'>" + g + "</svg><div class='legend'>" +
    series.map((s, i) => "<span><i style='background:" + PALETTE[i % PALETTE.length] + "'></i>" + esc(s.name) + "</span>").join("") +
    (limit && !drawLimit ? "<span>" + esc(limit.label) + "</span>" : "") + "</div>";
  document.getElementById("charts").appendChild(div);
}
function ticks(max, n) { const raw = max / n, p = Math.pow(10, Math.floor(Math.log10(raw))), step = [1, 2, 5, 10].map(m => m * p).find(s => s >= raw); const out = []; for (let v = 0; v <= max; v += step) out.push(+v.toFixed(6)); return out; }
function tick(v) { return v >= 1000 ? (v / 1000).toFixed(v % 1000 ? 1 : 0) + "k" : String(+v.toFixed(2)); }

chart("Server CPU", "cores", (r, s) => server(r, s).cores_avg, sh ? { value: sh.cpus, label: sh.cpus + " cores on the server" } : null);
chart("Server memory", "MB", (r, s) => server(r, s).rss_peak_mb, sh ? { value: sh.mem_mb, label: Math.round(sh.mem_mb / 1000) + " GB on the server" } : null);

// Section links, built last so the sections the notes and the machine
// recording add are in it too.
document.getElementById("toc").innerHTML = [...document.querySelectorAll("h2")].map(h => {
  h.id = h.id || h.textContent.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
  return "<a href='#" + h.id + "'>" + esc(h.textContent) + "</a>";
}).join("");
</script>
</body>
</html>
`
