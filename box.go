package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// BoxSample is the whole machine over one second: where every core's time
// went, memory, and the NIC. The process figures in Sample miss the kernel's
// share of the send path that runs outside the process.
type BoxSample struct {
	T         time.Time `json:"t"`
	Cores     []float64 `json:"cores"` // busy fraction per core
	User      float64   `json:"user"`  // fractions of all core time
	System    float64   `json:"system"`
	IRQ       float64   `json:"irq"` // hard and soft interrupts
	Steal     float64   `json:"steal"`
	MemUsedMB float64   `json:"mem_used_mb"`
	SockMemMB float64   `json:"sock_mem_mb"` // TCP send and receive buffers
	TxMbps    float64   `json:"tx_mbps"`
	RxMbps    float64   `json:"rx_mbps"`
	TxPps     float64   `json:"tx_pps"`
	Retrans   float64   `json:"retrans_per_s"`
}

// BoxStep is a BoxSample series over a step: averages, and peaks for memory.
type BoxStep struct {
	Cores     []float64 `json:"cores"`
	User      float64   `json:"user"`
	System    float64   `json:"system"`
	IRQ       float64   `json:"irq"`
	Steal     float64   `json:"steal"`
	MemUsedMB float64   `json:"mem_used_mb"`
	SockMemMB float64   `json:"sock_mem_mb"`
	TxMbps    float64   `json:"tx_mbps"`
	TxPps     float64   `json:"tx_pps"`
	Retrans   float64   `json:"retrans_per_s"`
}

type cpuTimes struct{ user, nice, system, idle, iowait, irq, softirq, steal int64 }

func (c cpuTimes) total() int64 {
	return c.user + c.nice + c.system + c.idle + c.iowait + c.irq + c.softirq + c.steal
}

func (c cpuTimes) sub(p cpuTimes) cpuTimes {
	return cpuTimes{c.user - p.user, c.nice - p.nice, c.system - p.system, c.idle - p.idle, c.iowait - p.iowait, c.irq - p.irq, c.softirq - p.softirq, c.steal - p.steal}
}

type boxCounters struct {
	t             time.Time
	all           cpuTimes
	cores         []cpuTimes
	txB, rxB, txP int64
	retrans       int64
}

type boxSampler struct{ prev boxCounters }

func readCounters(now time.Time) boxCounters {
	c := boxCounters{t: now}
	for _, line := range fileLines("/proc/stat") {
		f := strings.Fields(line)
		if len(f) < 9 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		var t cpuTimes
		for i, p := range []*int64{&t.user, &t.nice, &t.system, &t.idle, &t.iowait, &t.irq, &t.softirq, &t.steal} {
			*p, _ = strconv.ParseInt(f[i+1], 10, 64)
		}
		if f[0] == "cpu" {
			c.all = t
		} else {
			c.cores = append(c.cores, t)
		}
	}
	for _, line := range fileLines("/proc/net/dev") {
		name, rest, ok := strings.Cut(line, ":")
		f := strings.Fields(rest)
		if !ok || strings.TrimSpace(name) == "lo" || len(f) < 10 {
			continue
		}
		rx, _ := strconv.ParseInt(f[0], 10, 64)
		tx, _ := strconv.ParseInt(f[8], 10, 64)
		txp, _ := strconv.ParseInt(f[9], 10, 64)
		c.rxB, c.txB, c.txP = c.rxB+rx, c.txB+tx, c.txP+txp
	}
	// /proc/net/snmp has a header line of names and a line of values per protocol.
	var names []string
	for _, line := range fileLines("/proc/net/snmp") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "Tcp:" {
			continue
		}
		if names == nil {
			names = f
			continue
		}
		for i, n := range names {
			if n == "RetransSegs" && i < len(f) {
				c.retrans, _ = strconv.ParseInt(f[i], 10, 64)
			}
		}
	}
	return c
}

func (b *boxSampler) sample(now time.Time) BoxSample {
	cur := readCounters(now)
	prev := b.prev
	b.prev = cur
	s := BoxSample{T: now, Cores: []float64{}}
	if prev.t.IsZero() || len(prev.cores) != len(cur.cores) {
		return s
	}
	d := cur.all.sub(prev.all)
	if total := float64(d.total()); total > 0 {
		s.User = float64(d.user+d.nice) / total
		s.System = float64(d.system) / total
		s.IRQ = float64(d.irq+d.softirq) / total
		s.Steal = float64(d.steal) / total
	}
	for i, core := range cur.cores {
		dc := core.sub(prev.cores[i])
		var busy float64
		if total := dc.total(); total > 0 {
			busy = float64(total-dc.idle-dc.iowait) / float64(total)
		}
		s.Cores = append(s.Cores, busy)
	}
	if dt := now.Sub(prev.t).Seconds(); dt > 0 {
		s.TxMbps = float64(cur.txB-prev.txB) * 8 / 1e6 / dt
		s.RxMbps = float64(cur.rxB-prev.rxB) * 8 / 1e6 / dt
		s.TxPps = float64(cur.txP-prev.txP) / dt
		s.Retrans = float64(cur.retrans-prev.retrans) / dt
	}
	s.MemUsedMB, s.SockMemMB = readMemory()
	return s
}

func readMemory() (usedMB, sockMB float64) {
	var total, avail float64
	for _, line := range fileLines("/proc/meminfo") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.ParseFloat(f[1], 64)
		switch f[0] {
		case "MemTotal:":
			total = kb
		case "MemAvailable:":
			avail = kb
		}
	}
	usedMB = (total - avail) / 1024
	for _, line := range fileLines("/proc/net/sockstat") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "TCP:" {
			continue
		}
		for i := 1; i+1 < len(f); i += 2 {
			if f[i] == "mem" {
				pages, _ := strconv.ParseFloat(f[i+1], 64)
				sockMB = pages * float64(os.Getpagesize()) / 1024 / 1024
			}
		}
	}
	return
}

func fileLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(string(data), "\n")
}

// boxStep averages the samples inside a window; memory keeps its peak.
func boxStep(samples []BoxSample, start, end time.Time) *BoxStep {
	var b BoxStep
	n := 0
	for _, s := range samples {
		if s.T.Before(start) || s.T.After(end) || len(s.Cores) == 0 {
			continue
		}
		if b.Cores == nil {
			b.Cores = make([]float64, len(s.Cores))
		}
		for i := range b.Cores {
			if i < len(s.Cores) {
				b.Cores[i] += s.Cores[i]
			}
		}
		b.User += s.User
		b.System += s.System
		b.IRQ += s.IRQ
		b.Steal += s.Steal
		b.TxMbps += s.TxMbps
		b.TxPps += s.TxPps
		b.Retrans += s.Retrans
		b.MemUsedMB = max(b.MemUsedMB, s.MemUsedMB)
		b.SockMemMB = max(b.SockMemMB, s.SockMemMB)
		n++
	}
	if n == 0 {
		return nil
	}
	for i := range b.Cores {
		b.Cores[i] /= float64(n)
	}
	b.User, b.System, b.IRQ, b.Steal = b.User/float64(n), b.System/float64(n), b.IRQ/float64(n), b.Steal/float64(n)
	b.TxMbps, b.TxPps, b.Retrans = b.TxMbps/float64(n), b.TxPps/float64(n), b.Retrans/float64(n)
	return &b
}
