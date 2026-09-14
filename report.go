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

func reportCmd(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	merge := fs.String("merge", "", "serve.json recorded by icetest serve on the server box; its samples are folded into run.json")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: icetest report [--merge serve.json] <run-dir>")
	}
	dir := fs.Arg(0)
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return err
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	if *merge != "" {
		if err := mergeServe(&r, *merge); err != nil {
			return err
		}
		data, _ := json.MarshalIndent(r, "", " ")
		if err := os.WriteFile(filepath.Join(dir, "run.json"), data, 0o644); err != nil {
			return err
		}
	}
	if err := writeReports(&r, dir); err != nil {
		return err
	}
	fmt.Print(renderReport(&r))
	return nil
}

func writeReports(r *Run, dir string) error {
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(renderReport(r)), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.html"), []byte(renderHTML(r)), 0o644)
}

func renderReport(r *Run) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("# %s\n\n", r.Scenario)
	if r.Description != "" {
		w("%s\n\n", r.Description)
	}
	w("- started: %s, duration: %s\n", r.Started.Format("2006-01-02 15:04:05"), r.Ended.Sub(r.Started).Round(1e9))
	w("- host: %d cpus, %d MB, %s %s\n", r.Host.CPUs, r.Host.MemMB, r.Host.Kernel, r.Host.Arch)
	if r.Host.Liquidsoap != "" {
		w("- liquidsoap: %s\n", r.Host.Liquidsoap)
	}
	c := r.Config
	w("- ramp: %d +%d up to %d, hold %s, churn %s, icy %.0f%%, stall %s, fail threshold %.1f%%\n",
		c.Start, c.Step, c.Max, c.Hold, c.Listener.Churn, c.Listener.ICY*100, c.Listener.Stall, c.FailThreshold*100)
	if l := loadSummary(r); l != "" {
		w("- %s\n", l)
	}
	if c.Remote != "" {
		if r.ServerHost != nil {
			w("- remote server: %s, %d cpus, %d MB, %s %s", c.Remote, r.ServerHost.CPUs, r.ServerHost.MemMB, r.ServerHost.Kernel, r.ServerHost.Arch)
			if r.ServerHost.Liquidsoap != "" {
				w(", %s", r.ServerHost.Liquidsoap)
			}
			w("\n")
		} else {
			w("- remote server: %s (not sampled; merge a serve.json with icetest report --merge)\n", c.Remote)
		}
	}
	w("\n## Result\n\n")
	switch {
	case r.Ceiling == 0:
		w("**No step passed.**\n")
	case r.MaxReached:
		w("**Every step passed up to the configured maximum of %d listeners.** Raise --max to find the ceiling.\n", r.Ceiling)
	default:
		w("**Ceiling: %d listeners.** The next step failed.\n", r.Ceiling)
	}
	for _, s := range r.Steps {
		if !s.Passed {
			w("\nFirst failing step (%d listeners):\n", s.Target)
			for _, reason := range s.Reasons {
				w("- %s\n", reason)
			}
			break
		}
	}
	procs := procNames(r)
	w("\n## Steps\n\n")
	w("| listeners | ok | Mbit/s avg | Mbit/s peak | ttfb p50/p99 ms | failures | lag max |")
	for _, p := range procs {
		w(" %s cores | %s MB | %s fds |", p, p, p)
	}
	w(" sys cpu | canaries | alerts |\n")
	w("|---|---|---|---|---|---|---|")
	for range procs {
		w("---|---|---|")
	}
	w("---|---|---|\n")
	for _, s := range r.Steps {
		ok := "pass"
		if !s.Passed {
			ok = "FAIL"
		}
		var fails int64
		lag := 0
		for _, n := range s.Failures {
			fails += n
		}
		for _, m := range s.Mounts {
			lag = max(lag, m.MaxLagging)
		}
		w("| %d | %s | %.1f | %.1f | %.0f / %.0f | %d | %d |", s.PeakLive, ok, s.AvgMbps, s.PeakMbps, s.TTFBp50ms, s.TTFBp99ms, fails, lag)
		for _, p := range procs {
			ps := s.Processes[p]
			w(" %.2f | %.0f | %d |", ps.CoresAvg, ps.RSSPeakMB, ps.FdsPeak)
		}
		w(" %.0f%% | %d/%d | %d |\n", s.SystemCPU*100, s.CanaryOK, s.CanaryOK+s.CanaryFail, s.Alerts)
	}
	if len(r.Steps) > 0 {
		best := r.Steps[len(r.Steps)-1]
		for _, s := range r.Steps {
			if s.Passed {
				best = s
			}
		}
		w("\n## Mounts at %d listeners\n\n", best.PeakLive)
		w("| mount | live | connects | median kbps | min kbps | lag max | icy ok/bad | failures |\n|---|---|---|---|---|---|---|---|\n")
		mounts := make([]string, 0, len(best.Mounts))
		for m := range best.Mounts {
			mounts = append(mounts, m)
		}
		sort.Strings(mounts)
		for _, m := range mounts {
			ms := best.Mounts[m]
			w("| %s | %d | %d | %.0f | %.0f | %d | %d/%d | %s |\n", m, ms.Live, ms.Connects, ms.MedianKbps, ms.MinKbps, ms.MaxLagging, ms.MetaOK, ms.MetaBad, failureList(ms.Failures))
		}
	}
	var failed []Canary
	for _, cn := range r.Canaries {
		if !cn.OK {
			failed = append(failed, cn)
		}
	}
	if len(failed) > 0 {
		w("\n## Failed canaries\n\n")
		for _, cn := range failed {
			w("- step %d %s after %s: %s\n", cn.Step, cn.Mount, cn.Duration.Round(1e8), firstLine(cn.Error))
		}
	}
	if len(r.Alerts) > 0 {
		w("\n## Server log alerts\n\n")
		for i, a := range r.Alerts {
			if i == 20 {
				w("- ... %d more\n", len(r.Alerts)-20)
				break
			}
			w("- %s\n", a.Line)
		}
	}
	if len(r.Events) > 0 {
		w("\n## Listener events\n\n")
		for i, e := range r.Events {
			if i == 30 {
				w("- ... %d more in run.json\n", len(r.Events)-30)
				break
			}
			w("- %s\n", e)
		}
	}
	return b.String()
}

func failureList(f map[string]int64) string {
	if len(f) == 0 {
		return "-"
	}
	var parts []string
	for k, v := range f {
		parts = append(parts, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// The load generators are left out of the tables: their cost only matters
// as proof that a failure was the server's, which loadSummary states.
func isLoadGenerator(name string) bool { return strings.HasPrefix(name, "icetest") }

func procNames(r *Run) []string {
	seen := map[string]bool{}
	var names []string
	for _, s := range r.Steps {
		for p := range s.Processes {
			if !seen[p] && !isLoadGenerator(p) {
				seen[p] = true
				names = append(names, p)
			}
		}
	}
	sort.Strings(names)
	return names
}

// loadSummary is one line on the load generators: how many boxes and the
// busiest any of them got, so a failing step can be read against it.
func loadSummary(r *Run) string {
	boxes := map[string]bool{}
	var peak float64
	for _, s := range r.Steps {
		for p, ps := range s.Processes {
			if isLoadGenerator(p) {
				boxes[p] = true
				peak = max(peak, ps.CoresPeak)
			}
		}
	}
	if len(boxes) == 0 {
		return ""
	}
	return fmt.Sprintf("load generators: %d box(es), peak %.1f cores on the busiest", len(boxes), peak)
}
