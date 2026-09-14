package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// combineCmd folds the runs of several load boxes driving one server into a
// single run: steps are summed by index, ticks by second, and each box keeps
// its own cost column.
func combineCmd(args []string) error {
	fs := flag.NewFlagSet("combine", flag.ExitOnError)
	out := fs.String("o", "", "directory to write the combined run.json and reports into")
	fs.Parse(args)
	if *out == "" || fs.NArg() < 1 {
		return fmt.Errorf("usage: icetest combine -o <out-dir> <run-dir>...")
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
	c := combineRuns(runs)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(c, "", " ")
	if err := os.WriteFile(filepath.Join(*out, "run.json"), data, 0o644); err != nil {
		return err
	}
	if err := writeReports(c, *out); err != nil {
		return err
	}
	fmt.Print(renderReport(c))
	return nil
}

func combineRuns(runs []*Run) *Run {
	first := runs[0]
	c := &Run{
		Scenario:    first.Scenario,
		Description: first.Description,
		Host:        first.Host,
		Config:      first.Config,
		Started:     first.Started,
		Ended:       first.Ended,
		ServerHost:  first.ServerHost,
		Steps:       []StepResult{},
		Samples:     []Sample{},
		SysCPU:      []timedFloat{},
		Ticks:       []TickStat{},
		Alerts:      []Alert{},
		Canaries:    []Canary{},
		Events:      []string{},
	}
	c.Config.Start, c.Config.Step, c.Config.Max = 0, 0, 0
	c.Config.Listener.ConnectRate = 0
	steps := len(first.Steps)
	for i, r := range runs {
		c.Config.Start += r.Config.Start
		c.Config.Step += r.Config.Step
		c.Config.Max += r.Config.Max
		c.Config.Listener.ConnectRate += r.Config.Listener.ConnectRate
		if r.Started.Before(c.Started) {
			c.Started = r.Started
		}
		if r.Ended.After(c.Ended) {
			c.Ended = r.Ended
		}
		steps = min(steps, len(r.Steps))
		box := fmt.Sprintf("icetest-%d", i+1)
		for _, s := range r.Samples {
			if s.Process == "icetest" {
				s.Process = box
			}
			c.Samples = append(c.Samples, s)
		}
		c.Alerts = append(c.Alerts, r.Alerts...)
		c.Canaries = append(c.Canaries, r.Canaries...)
		for _, e := range r.Events {
			if len(c.Events) < logLimit {
				c.Events = append(c.Events, box+" "+e)
			}
		}
	}
	c.Ticks = combineTicks(runs)
	c.SysCPU = combineSysCPU(runs)
	failed := false
	for i := 0; i < steps; i++ {
		s := combineStep(runs, i)
		if s.Passed && !failed {
			c.Ceiling = s.Target
		}
		failed = failed || !s.Passed
		c.Steps = append(c.Steps, s)
	}
	c.MaxReached = !failed && steps > 0 && c.Steps[steps-1].Target >= c.Config.Max
	return c
}

func combineStep(runs []*Run, i int) StepResult {
	s := StepResult{Index: i, Passed: true, Failures: map[string]int64{}, Mounts: map[string]MountStep{}, Processes: map[string]ProcStep{}}
	var sysCPU float64
	for n, r := range runs {
		st := r.Steps[i]
		if n == 0 || st.Start.Before(s.Start) {
			s.Start = st.Start
		}
		if st.End.After(s.End) {
			s.End = st.End
		}
		s.Target += st.Target
		s.PeakLive += st.PeakLive
		s.AvgMbps += st.AvgMbps
		s.PeakMbps += st.PeakMbps
		s.CanaryOK += st.CanaryOK
		s.CanaryFail += st.CanaryFail
		s.Alerts += st.Alerts
		sysCPU += st.SystemCPU
		// The slowest box is what a listener there experienced.
		s.TTFBp50ms = max(s.TTFBp50ms, st.TTFBp50ms)
		s.TTFBp95ms = max(s.TTFBp95ms, st.TTFBp95ms)
		s.TTFBp99ms = max(s.TTFBp99ms, st.TTFBp99ms)
		if !st.Passed {
			s.Passed = false
			for _, reason := range st.Reasons {
				s.Reasons = append(s.Reasons, fmt.Sprintf("load box %d: %s", n+1, reason))
			}
		}
		for k, v := range st.Failures {
			s.Failures[k] += v
		}
		for name, p := range st.Processes {
			if name == "icetest" {
				name = fmt.Sprintf("icetest-%d", n+1)
			}
			s.Processes[name] = p
		}
		for path, m := range st.Mounts {
			acc := s.Mounts[path]
			if acc.Failures == nil {
				acc.Failures = map[string]int64{}
				acc.MinKbps = m.MinKbps
			}
			// Rates weighted by how many listeners each box had on the mount.
			acc.MedianKbps = (acc.MedianKbps*float64(acc.Live) + m.MedianKbps*float64(m.Live)) / float64(max(acc.Live+m.Live, 1))
			acc.MinKbps = min(acc.MinKbps, m.MinKbps)
			acc.Live += m.Live
			acc.Connects += m.Connects
			acc.MaxLagging += m.MaxLagging
			acc.MetaOK += m.MetaOK
			acc.MetaBad += m.MetaBad
			for k, v := range m.Failures {
				acc.Failures[k] += v
			}
			s.Mounts[path] = acc
		}
	}
	s.SystemCPU = sysCPU / float64(len(runs))
	return s
}

// combineTicks sums the per-mount ticks of every box that fell in the same
// second; a listener's rate is what its box measured, weighted by listeners.
func combineTicks(runs []*Run) []TickStat {
	type key struct {
		t     time.Time
		mount string
	}
	acc := map[key]TickStat{}
	for _, r := range runs {
		for _, t := range r.Ticks {
			k := key{t.T.Truncate(time.Second), t.Mount}
			a, seen := acc[k]
			if !seen {
				a = TickStat{T: k.t, Mount: t.Mount, MinBps: t.MinBps}
			}
			a.MedianBps = (a.MedianBps*float64(a.Live) + t.MedianBps*float64(t.Live)) / float64(max(a.Live+t.Live, 1))
			a.MinBps = min(a.MinBps, t.MinBps)
			a.Live += t.Live
			a.Bps += t.Bps
			a.Lagging += t.Lagging
			acc[k] = a
		}
	}
	out := make([]TickStat, 0, len(acc))
	for _, t := range acc {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].T.Equal(out[j].T) {
			return out[i].T.Before(out[j].T)
		}
		return out[i].Mount < out[j].Mount
	})
	return out
}

func combineSysCPU(runs []*Run) []timedFloat {
	sum := map[time.Time]float64{}
	count := map[time.Time]int{}
	for _, r := range runs {
		for _, v := range r.SysCPU {
			t := v.T.Truncate(time.Second)
			sum[t] += v.V
			count[t]++
		}
	}
	out := make([]timedFloat, 0, len(sum))
	for t, v := range sum {
		out = append(out, timedFloat{t, v / float64(count[t])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out
}
