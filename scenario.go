package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Mount is one endpoint the listeners pull from.
type Mount struct {
	Path        string  `json:"path"`
	Weight      float64 `json:"weight,omitempty"`       // share of the listener budget, default 1
	ContentType string  `json:"content_type,omitempty"` // when set, a different Content-Type fails the listener
	NominalKbps float64 `json:"nominal_kbps,omitempty"` // when set, a median rate below 90% of it fails the step
	When        string  `json:"when,omitempty"`         // only with --with <name>
}

// Process is a program the runner manages for the duration of the run.
type Process struct {
	Name    string     `json:"name"`
	Prepare [][]string `json:"prepare,omitempty"` // run to completion before Cmd starts
	Cmd     []string   `json:"cmd"`
	Server  bool       `json:"server,omitempty"` // its log is scanned for alerts
	When    string     `json:"when,omitempty"`   // only with --with <name>
}

type Scenario struct {
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	Templates     []string  `json:"templates,omitempty"` // files rendered with ${VAR} into the run dir
	Processes     []Process `json:"processes"`
	Mounts        []Mount   `json:"mounts"`
	AlertPatterns []string  `json:"alert_patterns,omitempty"`
	Dir           string    `json:"-"`
}

var defaultAlertPatterns = []string{
	"Latency is too high",
	"Too much latency",
	"possible source leak",
	"Fatal error",
	"failed while streaming",
}

// loadScenario reads a scenario, keeping only the mounts and processes whose
// "when" gate is absent or named in with.
func loadScenario(dir string, with []string) (*Scenario, error) {
	data, err := os.ReadFile(filepath.Join(dir, "scenario.json"))
	if err != nil {
		return nil, err
	}
	var s Scenario
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	enabled := map[string]bool{}
	for _, w := range with {
		enabled[w] = true
	}
	s.Mounts = slices.DeleteFunc(s.Mounts, func(m Mount) bool { return m.When != "" && !enabled[m.When] })
	s.Processes = slices.DeleteFunc(s.Processes, func(p Process) bool { return p.When != "" && !enabled[p.When] })
	if len(s.Mounts) == 0 {
		return nil, fmt.Errorf("%s: no mounts", dir)
	}
	for i := range s.Mounts {
		if s.Mounts[i].Weight == 0 {
			s.Mounts[i].Weight = 1
		}
	}
	if s.AlertPatterns == nil {
		s.AlertPatterns = defaultAlertPatterns
	}
	s.Dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if s.Name == "" {
		s.Name = filepath.Base(s.Dir)
	}
	return &s, nil
}

// Vars are the ${NAME} substitutions available to commands and templates.
// They are also exported to child processes as ICETEST_<NAME>.
type Vars map[string]string

func (v Vars) expand(s string) string {
	return os.Expand(s, func(name string) string { return v[name] })
}

func (v Vars) expandAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = v.expand(a)
	}
	return out
}

func (v Vars) env() []string {
	env := os.Environ()
	for k, val := range v {
		env = append(env, "ICETEST_"+k+"="+val)
	}
	return env
}

func renderTemplates(s *Scenario, v Vars, runDir string) error {
	for _, name := range s.Templates {
		data, err := os.ReadFile(filepath.Join(s.Dir, name))
		if err != nil {
			return err
		}
		out := filepath.Join(runDir, strings.TrimSuffix(name, ".tmpl"))
		if err := os.WriteFile(out, []byte(v.expand(string(data))), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// writeConcatList writes an ffmpeg concat demuxer playlist of every file in dir.
func writeConcatList(dir, out string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		// The concat demuxer unescapes ' as \'.
		fmt.Fprintf(&b, "file '%s'\n", strings.ReplaceAll(p, "'", `'\''`))
	}
	return os.WriteFile(out, []byte(b.String()), 0o644)
}
