package main

import (
	"testing"
	"time"
)

// A sampler that reads real counters must produce per-core fractions in
// [0,1] and non-zero memory on any Linux box.
func TestBoxSamplerReadsRealCounters(t *testing.T) {
	var b boxSampler
	b.sample(time.Now())
	time.Sleep(300 * time.Millisecond)
	s := b.sample(time.Now())
	if len(s.Cores) == 0 {
		t.Fatal("no cores read")
	}
	for i, c := range s.Cores {
		if c < 0 || c > 1 {
			t.Fatalf("core %d busy %v out of range", i, c)
		}
	}
	if s.MemUsedMB <= 0 || s.User+s.System+s.IRQ+s.Steal > 1.0001 {
		t.Fatalf("bad sample %+v", s)
	}
	st := boxStep([]BoxSample{s}, s.T.Add(-time.Second), s.T.Add(time.Second))
	if st == nil || len(st.Cores) != len(s.Cores) {
		t.Fatal("boxStep dropped the sample")
	}
	if boxStep([]BoxSample{s}, s.T.Add(time.Hour), s.T.Add(2*time.Hour)) != nil {
		t.Fatal("boxStep kept a sample outside the window")
	}
	t.Logf("cores %v user %.2f sys %.2f irq %.2f mem %.0f MB sock %.1f MB tx %.2f Mbit/s", s.Cores, s.User, s.System, s.IRQ, s.MemUsedMB, s.SockMemMB, s.TxMbps)
}
