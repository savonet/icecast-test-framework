package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	Scenario       string        `json:"scenario"`
	Remote         string        `json:"remote,omitempty"` // host:port of a server started elsewhere
	Port           int           `json:"port"`
	TLS            bool          `json:"tls"`
	Start          int           `json:"start"`
	Step           int           `json:"step"`
	Max            int           `json:"max"`
	Hold           time.Duration `json:"hold"`
	Warmup         time.Duration `json:"warmup"`
	StartupTimeout time.Duration `json:"startup_timeout"`
	FailThreshold  float64       `json:"fail_threshold"`
	CanaryDuration time.Duration `json:"canary_duration"`
	NoCanary       bool          `json:"no_canary"`
	CanaryLenient  bool          `json:"canary_lenient"`
	IgnoreAlerts   bool          `json:"ignore_alerts"`
	CanaryRetries  int           `json:"canary_retries"`
	With           []string      `json:"with,omitempty"` // enabled optional mounts and processes
	Env            []string      `json:"env,omitempty"`  // NAME=value pairs exported to the scenario as ICETEST_NAME
	Listener       ListenerOptions
	Liquidsoap     string `json:"liquidsoap"`
	AudioDir       string `json:"audio_dir"`
	Video          string `json:"video"`
	ResultsDir     string `json:"results_dir"`
}

type MountStep struct {
	Live       int              `json:"live"`
	Connects   int64            `json:"connects"`
	Failures   map[string]int64 `json:"failures"`
	MedianKbps float64          `json:"median_kbps"`
	MinKbps    float64          `json:"min_kbps"`
	MaxLagging int              `json:"max_lagging"`
	MetaOK     int64            `json:"meta_ok"`
	MetaBad    int64            `json:"meta_bad"`
}

type ProcStep struct {
	CoresAvg  float64 `json:"cores_avg"`
	CoresPeak float64 `json:"cores_peak"`
	RSSPeakMB float64 `json:"rss_peak_mb"`
	FdsPeak   int     `json:"fds_peak"`
	Threads   int     `json:"threads"`
}

type StepResult struct {
	Index      int                  `json:"index"`
	Target     int                  `json:"target"`
	Start      time.Time            `json:"start"`
	End        time.Time            `json:"end"`
	Passed     bool                 `json:"passed"`
	Reasons    []string             `json:"reasons,omitempty"`
	PeakLive   int                  `json:"peak_live"`
	Failures   map[string]int64     `json:"failures"`
	Mounts     map[string]MountStep `json:"mounts"`
	AvgMbps    float64              `json:"avg_mbps"`
	PeakMbps   float64              `json:"peak_mbps"`
	Processes  map[string]ProcStep  `json:"processes"`
	SystemCPU  float64              `json:"system_cpu"` // busy fraction of all cores
	TTFBp50ms  float64              `json:"ttfb_p50_ms"`
	TTFBp95ms  float64              `json:"ttfb_p95_ms"`
	TTFBp99ms  float64              `json:"ttfb_p99_ms"`
	CanaryOK   int                  `json:"canary_ok"`
	CanaryFail int                  `json:"canary_fail"`
	Alerts     int                  `json:"alerts"`
}

type Run struct {
	Scenario    string       `json:"scenario"`
	Description string       `json:"description"`
	Host        HostInfo     `json:"host"`
	Config      Config       `json:"config"`
	Started     time.Time    `json:"started"`
	Ended       time.Time    `json:"ended"`
	Steps       []StepResult `json:"steps"`
	Ceiling     int          `json:"ceiling"` // largest passing target, 0 if none
	MaxReached  bool         `json:"max_reached"`
	Samples     []Sample     `json:"samples"`
	SysCPU      []timedFloat `json:"system_cpu"`
	Ticks       []TickStat   `json:"ticks"`
	Alerts      []Alert      `json:"alerts"`
	Canaries    []Canary     `json:"canaries"`
	Events      []string     `json:"events"`
	ServerHost  *HostInfo    `json:"server_host,omitempty"` // merged from icetest serve
	ServerEnv   []string     `json:"server_env,omitempty"`  // the serve's --env, merged with it
}

type timedFloat struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

type HostInfo struct {
	CPUs       int    `json:"cpus"`
	MemMB      int    `json:"mem_mb"`
	Kernel     string `json:"kernel"`
	Arch       string `json:"arch"`
	Liquidsoap string `json:"liquidsoap,omitempty"`
}

func runCmd(args []string) error {
	var c Config
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.StringVar(&c.Remote, "remote", "", "host:port of an already running server; no process is started or sampled")
	fs.IntVar(&c.Port, "port", 8000, "port the scenario's server listens on")
	fs.BoolVar(&c.TLS, "tls", false, "connect with TLS")
	fs.IntVar(&c.Start, "start", 100, "listeners in the first step")
	fs.IntVar(&c.Step, "step", 100, "listeners added per step, 0 runs a single step")
	fs.IntVar(&c.Max, "max", 2000, "stop after the step reaching this many listeners")
	fs.DurationVar(&c.Hold, "hold", 30*time.Second, "time to hold each step once its target is reached")
	fs.DurationVar(&c.Warmup, "warmup", 10*time.Second, "idle time after the mounts come up")
	fs.DurationVar(&c.StartupTimeout, "startup-timeout", 90*time.Second, "time allowed for every mount to come up")
	fs.Float64Var(&c.FailThreshold, "fail-threshold", 0.01, "step fails above this fraction of failed listeners")
	fs.DurationVar(&c.CanaryDuration, "canary", 10*time.Second, "how long each ffmpeg canary decodes")
	fs.BoolVar(&c.NoCanary, "no-canary", false, "skip the ffmpeg canaries")
	fs.BoolVar(&c.CanaryLenient, "canary-lenient", false, "canaries tolerate decode errors after a successful open")
	fs.BoolVar(&c.IgnoreAlerts, "ignore-alerts", false, "server log alerts do not fail a step")
	fs.IntVar(&c.CanaryRetries, "canary-retries", 1, "extra attempts for a canary that fails to open the stream")
	var with string
	fs.StringVar(&with, "with", "", "comma-separated optional parts of the scenario to enable, e.g. FLAC; exported as ICETEST_<NAME>=1")
	var env envFlag
	fs.Var(&env, "env", "NAME=value exported to the scenario as ICETEST_NAME; repeatable")
	fs.StringVar(&c.Liquidsoap, "liquidsoap", "liquidsoap", "liquidsoap binary, exported as ${LIQUIDSOAP}")
	fs.StringVar(&c.AudioDir, "audio", "", "directory of audio files, exported as ${AUDIO_DIR}")
	fs.StringVar(&c.Video, "video", "", "video file, exported as ${VIDEO}")
	fs.StringVar(&c.ResultsDir, "results", "results", "where run directories are created")
	var binds string
	l := &c.Listener
	fs.StringVar(&binds, "bind", "", "comma-separated local IPs to spread connections over")
	fs.IntVar(&l.ConnectRate, "connect-rate", 100, "new connections per second")
	fs.DurationVar(&l.ConnectTimeout, "connect-timeout", 10*time.Second, "dial and response header deadline")
	fs.DurationVar(&l.Stall, "stall", 10*time.Second, "a listener receiving nothing for this long fails")
	fs.DurationVar(&l.Churn, "churn", 0, "mean listener session length, 0 keeps them connected")
	fs.Float64Var(&l.ICY, "icy", 0.5, "fraction of listeners requesting ICY metadata")
	fs.IntVar(&l.Window, "window", 10, "ticks (seconds) over which per-listener rate is measured")
	fs.Float64Var(&l.LagTolerance, "lag-tolerance", 0.2, "a listener below median*(1-tolerance) is lagging")
	fs.IntVar(&l.LagTicks, "lag-ticks", 5, "consecutive lagging ticks before a listener fails")
	fs.DurationVar(&l.RetryDelay, "retry-delay", 2*time.Second, "pause before a failed listener reconnects")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: icetest run [flags] <scenario-dir>")
	}
	c.Scenario = fs.Arg(0)
	c.Env = env
	if with != "" {
		c.With = strings.Split(with, ",")
	}
	l.Tick = time.Second
	l.TLS = c.TLS
	if binds != "" {
		l.Binds = strings.Split(binds, ",")
	}
	l.Host = c.Remote
	if l.Host == "" {
		l.Host = fmt.Sprintf("127.0.0.1:%d", c.Port)
	}
	// A listener's rate is measured once it is older than twice the window.
	if minHold := time.Duration(2*l.Window+5) * l.Tick; c.Hold < minHold {
		return fmt.Errorf("--hold %s is too short to measure listener rates over a %d s window; use at least %s", c.Hold, l.Window, minHold)
	}
	return run(&c)
}

func run(c *Config) error {
	sc, err := loadScenario(c.Scenario, c.With)
	if err != nil {
		return err
	}
	runDir, err := filepath.Abs(filepath.Join(c.ResultsDir, sc.Name, time.Now().Format("20060102-150405")))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	raiseNofile()
	vars, err := prepareRunDir(c, sc, runDir)
	if err != nil {
		return err
	}
	r := &Run{Scenario: sc.Name, Description: sc.Description, Config: *c, Started: time.Now(), Host: hostInfo(c),
		Steps: []StepResult{}, Samples: []Sample{}, SysCPU: []timedFloat{}, Ticks: []TickStat{}, Alerts: []Alert{}, Canaries: []Canary{}, Events: []string{}}
	fmt.Printf("run %s -> %s\n", sc.Name, runDir)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	sampler := newProcSampler()
	sampler.add("icetest", syscall.Getpgrp())
	var procs []*managed
	var scanner *logScanner
	defer func() { stopProcesses(procs) }()
	if c.Remote == "" {
		procs, scanner, err = startScenario(ctx, c, sc, vars, runDir, sampler)
		if err != nil {
			return err
		}
	}
	if err := waitForMounts(ctx, c, sc, procs); err != nil {
		return err
	}
	fmt.Printf("all mounts up, warming up for %s\n", c.Warmup)
	sleepCtx(ctx, c.Warmup)

	var mu sync.Mutex
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var sys systemCPU
		sys.sample()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				samples := sampler.sample(now)
				var alerts []Alert
				if scanner != nil {
					alerts = scanner.scan(now)
				}
				cpu := sys.sample()
				mu.Lock()
				r.Samples = append(r.Samples, samples...)
				r.Alerts = append(r.Alerts, alerts...)
				r.SysCPU = append(r.SysCPU, timedFloat{now, cpu})
				mu.Unlock()
			}
		}
	}()

	pool := newPool(c.Listener, sc.Mounts)
	pool.Start()
	url := "http://"
	if c.TLS {
		url = "https://"
	}
	url += c.Listener.Host

	for target, i := c.Start, 0; ctx.Err() == nil; target, i = target+c.Step, i+1 {
		if target > c.Max {
			target = c.Max
		}
		fmt.Printf("step %d: %d listeners\n", i, target)
		pool.SetTarget(target)
		before := pool.Snapshot()
		if !waitForTarget(ctx, pool, target, c) {
			fmt.Println("  target not reached")
		}
		start := time.Now()
		var canaries []Canary
		if !c.NoCanary {
			sleepCtx(ctx, c.Hold/2)
			var wg sync.WaitGroup
			var cmu sync.Mutex
			for _, m := range sc.Mounts {
				wg.Add(1)
				go func(m Mount) {
					defer wg.Done()
					// A join can land on a stream boundary; a retry tells that apart from a mount that is broken.
					for attempt := 0; attempt <= c.CanaryRetries; attempt++ {
						cn := runCanary(ctx, url+m.Path, i, m.Path, c.CanaryDuration, !c.CanaryLenient)
						cmu.Lock()
						canaries = append(canaries, cn)
						cmu.Unlock()
						if cn.OK || !strings.Contains(cn.Error, "Error opening input") {
							break
						}
					}
				}(m)
			}
			wg.Wait()
			if rest := c.Hold/2 - c.CanaryDuration; rest > 0 {
				sleepCtx(ctx, rest)
			}
		} else {
			sleepCtx(ctx, c.Hold)
		}
		end := time.Now()
		after := pool.Snapshot()
		mu.Lock()
		step := evaluateStep(r, c, sc, i, target, start, end, before, after, pool.Ticks(), canaries, procs)
		r.Canaries = append(r.Canaries, canaries...)
		r.Steps = append(r.Steps, step)
		mu.Unlock()
		if step.Passed {
			r.Ceiling = target
			fmt.Printf("  pass: %.1f Mbit/s, %s\n", step.AvgMbps, procSummary(step))
		} else {
			fmt.Printf("  FAIL: %s\n", strings.Join(step.Reasons, "; "))
			break
		}
		if target >= c.Max {
			r.MaxReached = true
			break
		}
		if c.Step == 0 {
			r.MaxReached = true
			break
		}
	}
	fmt.Println("stopping listeners")
	pool.Stop()
	cancel()
	<-monitorDone
	r.Ended = time.Now()
	r.Ticks = pool.Ticks()
	logMu.Lock()
	r.Events = append([]string(nil), logLines...)
	logMu.Unlock()

	data, _ := json.MarshalIndent(r, "", " ")
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), data, 0o644); err != nil {
		return err
	}
	if err := writeReports(r, runDir); err != nil {
		return err
	}
	fmt.Print("\n" + renderReport(r))
	fmt.Printf("report: %s\n", filepath.Join(runDir, "report.html"))
	return nil
}

type envFlag []string

func (e *envFlag) String() string     { return strings.Join(*e, ",") }
func (e *envFlag) Set(v string) error { *e = append(*e, v); return nil }

// prepareRunDir builds the scenario variables and the generated inputs
// (concat playlist, rendered templates) inside runDir.
func prepareRunDir(c *Config, sc *Scenario, runDir string) (Vars, error) {
	// Scenario processes run inside the scenario directory, so user paths
	// must not stay relative to where icetest was started.
	for _, path := range []*string{&c.AudioDir, &c.Video} {
		if *path == "" {
			continue
		}
		abs, err := filepath.Abs(*path)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, err
		}
		*path = abs
	}
	host, port, _ := strings.Cut(c.Listener.Host, ":")
	vars := Vars{
		"PORT": port, "HOST": host, "LIQUIDSOAP": c.Liquidsoap, "AUDIO_DIR": c.AudioDir, "VIDEO": c.Video,
		"RUN_DIR": runDir, "SCENARIO_DIR": sc.Dir, "AUDIO_CONCAT": filepath.Join(runDir, "audio-concat.txt"),
	}
	for _, w := range c.With {
		vars[w] = "1"
	}
	for _, kv := range c.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	if c.AudioDir != "" {
		if err := writeConcatList(c.AudioDir, vars["AUDIO_CONCAT"]); err != nil {
			return nil, err
		}
	}
	return vars, renderTemplates(sc, vars, runDir)
}

func procSummary(s StepResult) string {
	var parts []string
	names := make([]string, 0, len(s.Processes))
	for n := range s.Processes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := s.Processes[n]
		parts = append(parts, fmt.Sprintf("%s %.2f cores %.0f MB", n, p.CoresAvg, p.RSSPeakMB))
	}
	return strings.Join(parts, ", ")
}

func waitForTarget(ctx context.Context, pool *Pool, target int, c *Config) bool {
	live := 0
	for _, m := range pool.Snapshot() {
		live += m.Live
	}
	// Twice the time the missing listeners take to connect at the dial rate.
	missing := max(target-live, 0)
	deadline := time.Now().Add(2*time.Duration(missing/max(c.Listener.ConnectRate, 1))*time.Second + c.Listener.ConnectTimeout + 5*time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		live := 0
		for _, m := range pool.Snapshot() {
			live += m.Live
		}
		if live >= target {
			return true
		}
		sleepCtx(ctx, 200*time.Millisecond)
	}
	return false
}

func evaluateStep(r *Run, c *Config, sc *Scenario, i, target int, start, end time.Time, before, after map[string]MountSnapshot,
	ticks []TickStat, canaries []Canary, procs []*managed) StepResult {
	s := StepResult{Index: i, Target: target, Start: start, End: end, Failures: map[string]int64{},
		Mounts: map[string]MountStep{}, Processes: map[string]ProcStep{}}
	var ttfbs []time.Duration
	var failures int64
	for _, m := range sc.Mounts {
		b, a := before[m.Path], after[m.Path]
		ms := MountStep{Live: a.Live, Connects: a.Connects - b.Connects, Failures: map[string]int64{},
			MetaOK: a.MetaOK - b.MetaOK, MetaBad: a.MetaBad - b.MetaBad}
		for reason, n := range a.Ends {
			if d := n - b.Ends[reason]; d > 0 && !voluntaryEnds[reason] {
				ms.Failures[reason] = d
				s.Failures[reason] += d
				failures += d
			}
		}
		ttfbs = append(ttfbs, a.TTFBs[len(b.TTFBs):]...)
		var medians, mins []float64
		for _, t := range ticks {
			if t.Mount != m.Path || t.T.Before(start) || t.T.After(end) {
				continue
			}
			if t.Live > s.PeakLive {
				s.PeakLive = t.Live
			}
			if t.MedianBps > 0 {
				medians = append(medians, t.MedianBps)
				mins = append(mins, t.MinBps)
			}
			ms.MaxLagging = max(ms.MaxLagging, t.Lagging)
		}
		if len(medians) > 0 {
			ms.MedianKbps = median(medians) * 8 / 1000
			ms.MinKbps = median(mins) * 8 / 1000
		}
		if len(medians) > 0 && m.NominalKbps > 0 && ms.MedianKbps < 0.9*m.NominalKbps {
			s.Reasons = append(s.Reasons, fmt.Sprintf("%s median %.0f kbps below nominal %.0f", m.Path, ms.MedianKbps, m.NominalKbps))
		}
		if ms.MetaBad > 0 {
			s.Reasons = append(s.Reasons, fmt.Sprintf("%s: %d malformed ICY metadata blocks", m.Path, ms.MetaBad))
		}
		s.Mounts[m.Path] = ms
	}
	// Live is per mount, PeakLive across mounts is the sum at the busiest tick.
	perTick := map[time.Time]int{}
	for _, t := range ticks {
		if !t.T.Before(start) && !t.T.After(end) {
			perTick[t.T] += t.Live
		}
	}
	s.PeakLive = 0
	for _, n := range perTick {
		s.PeakLive = max(s.PeakLive, n)
	}
	if float64(failures) > c.FailThreshold*float64(max(s.PeakLive, 1)) {
		s.Reasons = append(s.Reasons, fmt.Sprintf("%d listeners failed out of %d (%v)", failures, s.PeakLive, s.Failures))
	}
	if s.PeakLive < target-int(c.FailThreshold*float64(target)) {
		s.Reasons = append(s.Reasons, fmt.Sprintf("reached %d of %d listeners", s.PeakLive, target))
	}
	// Throughput: sum over mounts per tick.
	bpsPerTick := map[time.Time]float64{}
	for _, t := range ticks {
		if !t.T.Before(start) && !t.T.After(end) {
			bpsPerTick[t.T] += t.Bps
		}
	}
	var sum float64
	for _, v := range bpsPerTick {
		sum += v
		s.PeakMbps = max(s.PeakMbps, v*8/1e6)
	}
	if len(bpsPerTick) > 0 {
		s.AvgMbps = sum / float64(len(bpsPerTick)) * 8 / 1e6
	}
	sort.Slice(ttfbs, func(a, b int) bool { return ttfbs[a] < ttfbs[b] })
	s.TTFBp50ms = percentile(ttfbs, 0.50)
	s.TTFBp95ms = percentile(ttfbs, 0.95)
	s.TTFBp99ms = percentile(ttfbs, 0.99)

	fillSampleStats(&s, r, start, end)
	lastByMount := map[string]Canary{}
	for _, cn := range canaries {
		if cn.OK {
			s.CanaryOK++
		} else {
			s.CanaryFail++
		}
		if prev, seen := lastByMount[cn.Mount]; !seen || cn.Start.After(prev.Start) {
			lastByMount[cn.Mount] = cn
		}
	}
	for _, cn := range lastByMount {
		if !cn.OK {
			s.Reasons = append(s.Reasons, fmt.Sprintf("canary %s: %s", cn.Mount, firstLine(cn.Error)))
		}
	}
	if s.Alerts > 0 && !c.IgnoreAlerts {
		s.Reasons = append(s.Reasons, fmt.Sprintf("%d server log alerts", s.Alerts))
	}
	for _, p := range procs {
		if err, gone := p.exited(); gone {
			s.Reasons = append(s.Reasons, fmt.Sprintf("process %s exited: %v", p.name, err))
		}
	}
	s.Passed = len(s.Reasons) == 0
	return s
}

// fillSampleStats derives the per-process, system CPU and alert figures of a
// step from the run's samples within the step window.
func fillSampleStats(s *StepResult, r *Run, start, end time.Time) {
	s.Processes = map[string]ProcStep{}
	counts := map[string]int{}
	for _, sm := range r.Samples {
		if sm.T.Before(start) || sm.T.After(end) {
			continue
		}
		p := s.Processes[sm.Process]
		p.CoresAvg += sm.Cores
		p.CoresPeak = max(p.CoresPeak, sm.Cores)
		p.RSSPeakMB = max(p.RSSPeakMB, sm.RSSMB)
		p.FdsPeak = max(p.FdsPeak, sm.Fds)
		p.Threads = sm.Threads
		s.Processes[sm.Process] = p
		counts[sm.Process]++
	}
	for name, p := range s.Processes {
		p.CoresAvg /= float64(counts[name])
		s.Processes[name] = p
	}
	var cpuSum float64
	var cpuN int
	for _, v := range r.SysCPU {
		if !v.T.Before(start) && !v.T.After(end) {
			cpuSum += v.V
			cpuN++
		}
	}
	s.SystemCPU = 0
	if cpuN > 0 {
		s.SystemCPU = cpuSum / float64(cpuN)
	}
	s.Alerts = 0
	for _, a := range r.Alerts {
		if !a.T.Before(start) && !a.T.After(end) {
			s.Alerts++
		}
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func percentile(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := min(int(float64(len(sorted))*p), len(sorted)-1)
	return float64(sorted[i]) / float64(time.Millisecond)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

type managed struct {
	name    string
	cmd     *exec.Cmd
	logPath string
	done    chan struct{} // closed once the process has exited
	err     error
}

// exited reports whether the process is gone, and how it ended.
func (m *managed) exited() (error, bool) {
	select {
	case <-m.done:
		return m.err, true
	default:
		return nil, false
	}
}

func startProcess(p Process, vars Vars, dir, runDir string) (*managed, error) {
	for i, prep := range p.Prepare {
		args := vars.expandAll(prep)
		fmt.Printf("prepare %s: %s\n", p.Name, strings.Join(args, " "))
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = vars.env()
		out, err := cmd.CombinedOutput()
		os.WriteFile(filepath.Join(runDir, fmt.Sprintf("%s-prepare-%d.log", p.Name, i)), out, 0o644)
		if err != nil {
			return nil, fmt.Errorf("prepare %s: %w\n%s", p.Name, err, out)
		}
	}
	args := vars.expandAll(p.Cmd)
	fmt.Printf("start %s: %s\n", p.Name, strings.Join(args, " "))
	m := &managed{name: p.Name, logPath: filepath.Join(runDir, p.Name+".log"), done: make(chan struct{})}
	logFile, err := os.Create(m.logPath)
	if err != nil {
		return nil, err
	}
	m.cmd = exec.Command(args[0], args[1:]...)
	m.cmd.Dir = dir
	m.cmd.Env = vars.env()
	m.cmd.Stdout = logFile
	m.cmd.Stderr = logFile
	// Its own process group, so ffmpeg children die with it.
	m.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := m.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", p.Name, err)
	}
	go func() {
		m.err = m.cmd.Wait()
		logFile.Close()
		close(m.done)
	}()
	return m, nil
}

func stopProcesses(procs []*managed) {
	for i := len(procs) - 1; i >= 0; i-- {
		m := procs[i]
		if _, gone := m.exited(); gone {
			continue
		}
		syscall.Kill(-m.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-m.done:
		case <-time.After(15 * time.Second):
			fmt.Printf("%s did not stop on SIGTERM, killing\n", m.name)
			syscall.Kill(-m.cmd.Process.Pid, syscall.SIGKILL)
			<-m.done
		}
	}
}

func waitForMounts(ctx context.Context, c *Config, sc *Scenario, procs []*managed) error {
	deadline := time.Now().Add(c.StartupTimeout)
	probe := newPool(c.Listener, nil)
	for _, m := range sc.Mounts {
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			for _, p := range procs {
				if err, gone := p.exited(); gone {
					return fmt.Errorf("process %s exited during startup: %v (see %s)", p.name, err, p.logPath)
				}
			}
			err := probeMount(probe, m)
			if err == nil {
				fmt.Printf("mount %s is up\n", m.Path)
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("mount %s not up after %s: %v", m.Path, c.StartupTimeout, err)
			}
			sleepCtx(ctx, time.Second)
		}
	}
	return nil
}

// startScenario launches every process of the scenario in order, registers it
// with the sampler, and returns the log scanner of the server process.
func startScenario(ctx context.Context, c *Config, sc *Scenario, vars Vars, runDir string, sampler *procSampler) ([]*managed, *logScanner, error) {
	var procs []*managed
	var scanner *logScanner
	for _, p := range sc.Processes {
		m, err := startProcess(p, vars, sc.Dir, runDir)
		if err != nil {
			return procs, nil, err
		}
		procs = append(procs, m)
		sampler.add(p.Name, m.cmd.Process.Pid)
		if p.Server {
			scanner = &logScanner{path: m.logPath, patterns: sc.AlertPatterns, limit: 200}
			// Source clients started next need something to connect to.
			if err := waitForPort(ctx, c, m); err != nil {
				return procs, nil, err
			}
		}
	}
	return procs, scanner, nil
}

func waitForPort(ctx context.Context, c *Config, server *managed) error {
	deadline := time.Now().Add(c.StartupTimeout)
	for ctx.Err() == nil {
		if err, gone := server.exited(); gone {
			return fmt.Errorf("process %s exited during startup: %v (see %s)", server.name, err, server.logPath)
		}
		conn, err := net.DialTimeout("tcp", c.Listener.Host, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not listening on %s after %s", server.name, c.Listener.Host, c.StartupTimeout)
		}
		sleepCtx(ctx, 500*time.Millisecond)
	}
	return ctx.Err()
}

// probeMount fetches the first body byte of a mount.
func probeMount(p *Pool, m Mount) error {
	conn, err := p.dial()
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET %s HTTP/1.0\r\nHost: %s\r\nUser-Agent: icetest-probe/0.1\r\n\r\n", m.Path, p.opts.Host)
	buf := make([]byte, 8192)
	headers, rest, err := readHeaders(conn, buf)
	if err != nil {
		return err
	}
	status, _ := parseHeaders(headers)
	if !strings.Contains(status, " 200") {
		return errors.New(status)
	}
	if len(rest) == 0 {
		if _, err := conn.Read(buf); err != nil {
			return err
		}
	}
	return nil
}

func raiseNofile() {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err == nil && lim.Cur < lim.Max {
		lim.Cur = lim.Max
		syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
	}
}

func hostInfo(c *Config) HostInfo {
	h := HostInfo{CPUs: runtime.NumCPU(), Arch: runtime.GOARCH}
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		h.Kernel = strings.TrimSpace(string(data))
	}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
				kb, _ := strconv.Atoi(f[1])
				h.MemMB = kb / 1024
			}
		}
	}
	if out, err := exec.Command(c.Liquidsoap, "--version").Output(); err == nil {
		h.Liquidsoap = firstLine(string(out))
	}
	return h
}
