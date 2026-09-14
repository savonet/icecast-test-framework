package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Serve is what icetest serve records: the server side of a two-machine run,
// merged into the load generator's run.json by icetest report --merge.
type Serve struct {
	Scenario string       `json:"scenario"`
	Host     HostInfo     `json:"host"`
	Started  time.Time    `json:"started"`
	Ended    time.Time    `json:"ended"`
	Samples  []Sample     `json:"samples"`
	SysCPU   []timedFloat `json:"system_cpu"`
	Box      []BoxSample  `json:"box"`
	Alerts   []Alert      `json:"alerts"`
	Env      []string     `json:"env,omitempty"`
}

func serveCmd(args []string) error {
	var c Config
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.IntVar(&c.Port, "port", 8000, "port the scenario's server listens on")
	fs.DurationVar(&c.StartupTimeout, "startup-timeout", 90*time.Second, "time allowed for every mount to come up")
	fs.StringVar(&c.Liquidsoap, "liquidsoap", "liquidsoap", "liquidsoap binary, exported as ${LIQUIDSOAP}")
	fs.StringVar(&c.AudioDir, "audio", "", "directory of audio files, exported as ${AUDIO_DIR}")
	fs.StringVar(&c.Video, "video", "", "video file, exported as ${VIDEO}")
	fs.StringVar(&c.ResultsDir, "results", "results", "where the serve directory is created")
	var with string
	fs.StringVar(&with, "with", "", "comma-separated optional parts of the scenario to enable, e.g. FLAC; exported as ICETEST_<NAME>=1")
	var env envFlag
	fs.Var(&env, "env", "NAME=value exported to the scenario as ICETEST_NAME; repeatable")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: icetest serve [flags] <scenario-dir>")
	}
	c.Scenario = fs.Arg(0)
	c.Env = env
	if with != "" {
		c.With = strings.Split(with, ",")
	}
	c.Listener.Host = fmt.Sprintf("127.0.0.1:%d", c.Port)
	c.Listener.ConnectTimeout = 10 * time.Second
	return serve(&c)
}

func serve(c *Config) error {
	sc, err := loadScenario(c.Scenario, c.With)
	if err != nil {
		return err
	}
	runDir, err := filepath.Abs(filepath.Join(c.ResultsDir, sc.Name, "serve-"+time.Now().Format("20060102-150405")))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	raiseNofile()
	// A leftover server from an earlier run would answer the probes in place
	// of the one about to start.
	if conn, err := net.DialTimeout("tcp", c.Listener.Host, time.Second); err == nil {
		conn.Close()
		return fmt.Errorf("%s is already accepting connections: stop whatever holds the port before serving", c.Listener.Host)
	}
	vars, err := prepareRunDir(c, sc, runDir)
	if err != nil {
		return err
	}
	fmt.Printf("serve %s -> %s\n", sc.Name, runDir)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	sampler := newProcSampler()
	procs, scanner, err := startScenario(ctx, c, sc, vars, runDir, sampler)
	defer func() { stopProcesses(procs) }()
	if err != nil {
		return err
	}
	if err := waitForMounts(ctx, c, sc, procs); err != nil {
		return err
	}
	// The marker is what a driver script waits for before starting the load.
	if err := os.WriteFile(filepath.Join(runDir, "ready"), nil, 0o644); err != nil {
		return err
	}
	fmt.Println("all mounts up, recording until SIGINT or SIGTERM")

	s := &Serve{Scenario: sc.Name, Host: hostInfo(c), Started: time.Now(), Samples: []Sample{}, SysCPU: []timedFloat{}, Box: []BoxSample{}, Alerts: []Alert{}, Env: c.Env}
	var sys systemCPU
	sys.sample()
	var box boxSampler
	box.sample(time.Now())
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case now := <-t.C:
			s.Samples = append(s.Samples, sampler.sample(now)...)
			if scanner != nil {
				s.Alerts = append(s.Alerts, scanner.scan(now)...)
			}
			s.SysCPU = append(s.SysCPU, timedFloat{now, sys.sample()})
			s.Box = append(s.Box, box.sample(now))
			for _, p := range procs {
				if err, gone := p.exited(); gone {
					return fmt.Errorf("process %s exited: %v (see %s)", p.name, err, p.logPath)
				}
			}
		}
	}
	s.Ended = time.Now()
	data, _ := json.MarshalIndent(s, "", " ")
	out := filepath.Join(runDir, "serve.json")
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("\nwrote %s (%d samples, %d alerts)\n", out, len(s.Samples), len(s.Alerts))
	return nil
}

// mergeServe folds a server-side recording into a run made with --remote and
// recomputes every figure derived from samples.
func mergeServe(r *Run, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var s Serve
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	r.Samples = append(r.Samples, s.Samples...)
	r.SysCPU = s.SysCPU
	r.Box = s.Box
	r.Alerts = append(r.Alerts, s.Alerts...)
	r.ServerHost = &s.Host
	r.ServerEnv = s.Env
	// The run judged its steps without the server log; alerts can now fail a
	// step and move the ceiling.
	r.Ceiling = 0
	r.MaxReached = false
	failed := false
	for i := range r.Steps {
		st := &r.Steps[i]
		fillSampleStats(st, r, st.Start, st.End)
		if st.Alerts > 0 && !r.Config.IgnoreAlerts && st.Passed {
			st.Passed = false
			st.Reasons = append(st.Reasons, fmt.Sprintf("%d server log alerts", st.Alerts))
		}
		if st.Passed && !failed {
			r.Ceiling = st.Target
		}
		failed = failed || !st.Passed
	}
	r.MaxReached = !failed && len(r.Steps) > 0 && r.Steps[len(r.Steps)-1].Target >= r.Config.Max
	return nil
}
