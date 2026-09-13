package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Sample is one process measurement.
type Sample struct {
	T       time.Time `json:"t"`
	Process string    `json:"process"`
	Cores   float64   `json:"cores"` // CPU time consumed per wall second
	RSSMB   float64   `json:"rss_mb"`
	Threads int       `json:"threads"`
	Fds     int       `json:"fds"`
}

// clockTicksPerSecond is Linux's USER_HZ as exposed in /proc. It has been 100
// on every mainstream distribution for decades; getconf CLK_TCK confirms.
const clockTicksPerSecond = 100

// procSampler measures process groups: a managed command may be a wrapper
// (opam exec, dune exec) or spawn workers, and the group is what costs.
type procSampler struct {
	pgids map[string]int
	prev  map[string]int64 // cpu ticks at the previous sample
	last  time.Time
}

func newProcSampler() *procSampler {
	return &procSampler{pgids: map[string]int{}, prev: map[string]int64{}}
}

func (s *procSampler) add(name string, pgid int) { s.pgids[name] = pgid }

// groupPids lists the processes whose process group is pgid.
func groupPids(pgid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if fields, ok := statFields(pid); ok && fields[2] == strconv.Itoa(pgid) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// statFields returns the fields of /proc/pid/stat after the command name,
// so fields[0] is the state, fields[2] the pgrp, fields[11] and [12] the CPU ticks.
func statFields(pid int) ([]string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, false
	}
	// The command name is parenthesised and may contain spaces.
	i := bytes.LastIndexByte(data, ')')
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 13 {
		return nil, false
	}
	return fields, true
}

func (s *procSampler) sample(now time.Time) []Sample {
	var out []Sample
	elapsed := now.Sub(s.last).Seconds()
	for name, pgid := range s.pgids {
		pids := groupPids(pgid)
		if len(pids) == 0 {
			continue
		}
		sm := Sample{T: now, Process: name}
		var ticks int64
		for _, pid := range pids {
			t, _ := cpuTicks(pid)
			ticks += t
			rss, threads := statusFields(pid)
			sm.RSSMB += rss
			sm.Threads += threads
			if entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid)); err == nil {
				sm.Fds += len(entries)
			}
		}
		if prev, seen := s.prev[name]; seen && elapsed > 0 {
			sm.Cores = float64(ticks-prev) / clockTicksPerSecond / elapsed
		}
		s.prev[name] = ticks
		out = append(out, sm)
	}
	s.last = now
	return out
}

func cpuTicks(pid int) (int64, bool) {
	fields, ok := statFields(pid)
	if !ok {
		return 0, false
	}
	utime, _ := strconv.ParseInt(fields[11], 10, 64)
	stime, _ := strconv.ParseInt(fields[12], 10, 64)
	return utime + stime, true
}

func statusFields(pid int) (rssMB float64, threads int) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ":")
		v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB"))
		switch k {
		case "VmRSS":
			kb, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
			rssMB = kb / 1024
		case "Threads":
			threads, _ = strconv.Atoi(v)
		}
	}
	return
}

// systemCPU returns the busy fraction of all cores since the previous call.
type systemCPU struct{ busy, total int64 }

func (c *systemCPU) sample() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	fields := strings.Fields(strings.SplitN(string(data), "\n", 2)[0])
	var total, idle int64
	for i, f := range fields[1:] {
		v, _ := strconv.ParseInt(f, 10, 64)
		total += v
		if i == 3 || i == 4 { // idle, iowait
			idle += v
		}
	}
	busy := total - idle
	var frac float64
	if dt := total - c.total; dt > 0 && c.total > 0 {
		frac = float64(busy-c.busy) / float64(dt)
	}
	c.busy, c.total = busy, total
	return frac
}

// Alert is a server log line matching one of the scenario's patterns.
type Alert struct {
	T    time.Time `json:"t"`
	Line string    `json:"line"`
}

type logScanner struct {
	path     string
	offset   int64
	patterns []string
	limit    int
}

func (s *logScanner) scan(now time.Time) []Alert {
	f, err := os.Open(s.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(s.offset, 0); err != nil {
		return nil
	}
	var out []Alert
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		s.offset += int64(len(line)) + 1
		for _, p := range s.patterns {
			if strings.Contains(line, p) {
				if len(out) < s.limit {
					out = append(out, Alert{T: now, Line: line})
				}
				break
			}
		}
	}
	return out
}
