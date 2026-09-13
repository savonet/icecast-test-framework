package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Why a listener ended. Reasons in voluntaryEnds are not failures.
const (
	endSession     = "session-end" // churn: the listener hung up on purpose
	endStopped     = "stopped"     // the run is over
	endEOF         = "eof"         // server closed the connection
	endStall       = "stall"       // no byte for --stall
	endLagging     = "lagging"     // received less than the mount median for --lag-ticks ticks
	endConnect     = "connect-error"
	endHTTP        = "http-error"
	endContentType = "content-type"
	endReadError   = "read-error"
)

var voluntaryEnds = map[string]bool{endSession: true, endStopped: true}

type ListenerOptions struct {
	Host           string
	TLS            bool
	Binds          []string
	ConnectRate    int           // dials per second across all mounts
	ConnectTimeout time.Duration // dial + response headers
	Stall          time.Duration
	Churn          time.Duration // mean session length, 0 = stay connected
	ICY            float64       // fraction of listeners requesting ICY metadata
	Tick           time.Duration
	Window         int           // ticks over which per-listener rate is measured
	LagTolerance   float64       // a rate below median*(1-tol) is lagging
	LagTicks       int           // consecutive lagging ticks before the listener is failed
	RetryDelay     time.Duration // pause before a failed listener is replaced
}

type listener struct {
	mount      *mountState
	conn       net.Conn
	started    time.Time
	bytes      atomic.Int64
	ttfb       atomic.Int64 // nanoseconds from dial, 0 until the first body byte
	voluntary  atomic.Bool
	lagFailed  atomic.Bool
	registered bool // in mount.live
	icy        bool
	metaOK     int
	metaBad    int

	// Owned by the sampler goroutine.
	hist    []int64
	ticks   int
	lagging int
}

type mountState struct {
	Mount
	target atomic.Int64
	total  atomic.Int64 // stream bytes received by every listener, ever

	mu       sync.Mutex
	live     map[*listener]struct{}
	pending  int // dialed, headers not read yet; counted so the maintainer does not overshoot
	retrying int // failed listeners waiting out RetryDelay, still owed to the target

	connects int64
	ends     map[string]int64
	ttfbs    []time.Duration
	metaOK   atomic.Int64
	metaBad  atomic.Int64
	wake     chan struct{}
}

// TickStat is the per-mount picture the sampler takes every tick.
type TickStat struct {
	T         time.Time `json:"t"`
	Mount     string    `json:"mount"`
	Live      int       `json:"live"`
	Bps       float64   `json:"bps"`        // aggregate bytes/s on this mount over the last tick
	MedianBps float64   `json:"median_bps"` // per-listener, over the window
	MinBps    float64   `json:"min_bps"`
	Lagging   int       `json:"lagging"`
}

type Pool struct {
	opts     ListenerOptions
	mounts   []*mountState
	stopping atomic.Bool
	wg       sync.WaitGroup
	dialTok  chan struct{}
	bindIdx  atomic.Int64

	mu    sync.Mutex
	ticks []TickStat
	done  chan struct{}
}

func newPool(opts ListenerOptions, mounts []Mount) *Pool {
	p := &Pool{opts: opts, done: make(chan struct{})}
	for _, m := range mounts {
		p.mounts = append(p.mounts, &mountState{
			Mount: m,
			live:  map[*listener]struct{}{},
			ends:  map[string]int64{},
			wake:  make(chan struct{}, 1),
		})
	}
	return p
}

func (p *Pool) Start() {
	p.dialTok = make(chan struct{}, p.opts.ConnectRate)
	go func() {
		t := time.NewTicker(time.Second / time.Duration(p.opts.ConnectRate))
		defer t.Stop()
		for {
			select {
			case <-t.C:
				select {
				case p.dialTok <- struct{}{}:
				default:
				}
			case <-p.done:
				return
			}
		}
	}()
	for _, m := range p.mounts {
		go p.maintain(m)
	}
	go p.sample()
}

// SetTarget spreads n listeners over the mounts by weight.
func (p *Pool) SetTarget(n int) {
	var total float64
	for _, m := range p.mounts {
		total += m.Weight
	}
	assigned := 0
	for i, m := range p.mounts {
		share := int(float64(n) * m.Weight / total)
		if i == len(p.mounts)-1 {
			share = n - assigned
		}
		assigned += share
		m.target.Store(int64(share))
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
}

func (p *Pool) Stop() {
	p.stopping.Store(true)
	close(p.done)
	for _, m := range p.mounts {
		m.mu.Lock()
		for l := range m.live {
			l.conn.Close()
		}
		m.mu.Unlock()
	}
	p.wg.Wait()
}

func (p *Pool) maintain(m *mountState) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for !p.stopping.Load() {
		m.mu.Lock()
		missing := int(m.target.Load()) - len(m.live) - m.pending - m.retrying
		if missing > 0 {
			m.pending++
		}
		m.mu.Unlock()
		if missing <= 0 {
			select {
			case <-t.C:
			case <-m.wake:
			case <-p.done:
			}
			continue
		}
		select {
		case <-p.dialTok:
		case <-p.done:
			m.mu.Lock()
			m.pending--
			m.mu.Unlock()
			return
		}
		l := &listener{mount: m, icy: rand.Float64() < p.opts.ICY, hist: make([]int64, p.opts.Window+1)}
		p.wg.Add(1)
		go p.run(l)
	}
}

func (p *Pool) run(l *listener) {
	defer p.wg.Done()
	m := l.mount
	l.started = time.Now()
	reason := p.stream(l)
	if p.stopping.Load() && voluntaryEnds[reason] == false && reason != endConnect {
		reason = endStopped
	}
	failed := !voluntaryEnds[reason]
	m.mu.Lock()
	if l.registered {
		delete(m.live, l)
	} else {
		m.pending--
	}
	m.ends[reason]++
	if failed {
		m.retrying++
	}
	m.mu.Unlock()
	if failed {
		// A real client pauses before reconnecting; without this a rejected
		// mount turns into a connect storm that inflates the failure count.
		select {
		case <-time.After(p.opts.RetryDelay):
		case <-p.done:
		}
		m.mu.Lock()
		m.retrying--
		m.mu.Unlock()
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (p *Pool) dial() (net.Conn, error) {
	d := net.Dialer{Timeout: p.opts.ConnectTimeout}
	if n := len(p.opts.Binds); n > 0 {
		ip := p.opts.Binds[int(p.bindIdx.Add(1))%n]
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(ip)}
	}
	conn, err := d.Dial("tcp", p.opts.Host)
	if err != nil || !p.opts.TLS {
		return conn, err
	}
	host, _, _ := net.SplitHostPort(p.opts.Host)
	tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: host})
	tc.SetDeadline(time.Now().Add(p.opts.ConnectTimeout))
	if err := tc.Handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	return tc, nil
}

func (p *Pool) stream(l *listener) string {
	m := l.mount
	conn, err := p.dial()
	if err != nil {
		if p.stopping.Load() {
			return endStopped
		}
		logf("%s: %s: %v", m.Path, endConnect, err)
		return endConnect
	}
	defer conn.Close()
	l.conn = conn

	req := fmt.Sprintf("GET %s HTTP/1.0\r\nHost: %s\r\nUser-Agent: icetest/0.1\r\nAccept: */*\r\n", m.Path, p.opts.Host)
	if l.icy {
		req += "Icy-MetaData: 1\r\n"
	}
	req += "\r\n"
	conn.SetDeadline(time.Now().Add(p.opts.ConnectTimeout))
	if _, err := io.WriteString(conn, req); err != nil {
		return endConnect
	}
	buf := make([]byte, 8192)
	headers, rest, err := readHeaders(conn, buf)
	if err != nil {
		if p.stopping.Load() {
			return endStopped
		}
		logf("%s: %s: %v", m.Path, endHTTP, err)
		return endHTTP
	}
	status, hdr := parseHeaders(headers)
	if !strings.Contains(status, " 200") {
		logf("%s: %s: %q", m.Path, endHTTP, status)
		return endHTTP
	}
	if m.ContentType != "" && !strings.HasPrefix(hdr["content-type"], m.ContentType) {
		logf("%s: %s: got %q want %q", m.Path, endContentType, hdr["content-type"], m.ContentType)
		return endContentType
	}
	var icy *icyState
	if l.icy {
		if n, _ := strconv.Atoi(hdr["icy-metaint"]); n > 0 {
			icy = &icyState{metaint: n, untilMeta: n}
		}
	}
	m.mu.Lock()
	m.connects++
	m.live[l] = struct{}{}
	m.pending--
	l.registered = true
	m.mu.Unlock()

	if p.opts.Churn > 0 {
		session := time.Duration(rand.ExpFloat64() * float64(p.opts.Churn))
		timer := time.AfterFunc(session, func() {
			l.voluntary.Store(true)
			conn.Close()
		})
		defer timer.Stop()
	}

	first := true
	for {
		if len(rest) == 0 {
			conn.SetReadDeadline(time.Now().Add(p.opts.Stall))
			n, err := conn.Read(buf)
			if err != nil {
				switch {
				case p.stopping.Load():
					return endStopped
				case l.voluntary.Load():
					return endSession
				case l.lagFailed.Load():
					return endLagging
				case errors.Is(err, io.EOF):
					return endEOF
				case errors.Is(err, os.ErrDeadlineExceeded):
					return endStall
				default:
					logf("%s: %s: %v", m.Path, endReadError, err)
					return endReadError
				}
			}
			rest = buf[:n]
		}
		if first {
			first = false
			t := time.Since(l.started)
			l.ttfb.Store(int64(t))
			m.mu.Lock()
			m.ttfbs = append(m.ttfbs, t)
			m.mu.Unlock()
		}
		n := len(rest)
		if icy != nil {
			n = icy.feed(rest, l)
		}
		l.bytes.Add(int64(n))
		m.total.Add(int64(n))
		rest = nil
	}
}

// readHeaders reads until the blank line and returns the header block and
// whatever body bytes came with it. buf must be large enough for the headers.
func readHeaders(conn net.Conn, buf []byte) (string, []byte, error) {
	filled := 0
	for {
		if filled == len(buf) {
			return "", nil, errors.New("headers too long")
		}
		n, err := conn.Read(buf[filled:])
		if err != nil {
			return "", nil, err
		}
		filled += n
		if i := bytes.Index(buf[:filled], []byte("\r\n\r\n")); i >= 0 {
			rest := append([]byte(nil), buf[i+4:filled]...)
			return string(buf[:i]), rest, nil
		}
	}
}

func parseHeaders(block string) (string, map[string]string) {
	lines := strings.Split(block, "\r\n")
	hdr := map[string]string{}
	for _, line := range lines[1:] {
		if k, v, ok := strings.Cut(line, ":"); ok {
			hdr[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return lines[0], hdr
}

// icyState strips interleaved ICY metadata blocks out of the byte stream.
type icyState struct {
	metaint       int
	untilMeta     int
	metaRemaining int
	meta          []byte
}

// feed consumes b and returns how many bytes of it were stream data.
func (s *icyState) feed(b []byte, l *listener) int {
	stream := 0
	for len(b) > 0 {
		switch {
		case s.metaRemaining > 0:
			take := min(len(b), s.metaRemaining)
			s.meta = append(s.meta, b[:take]...)
			b = b[take:]
			s.metaRemaining -= take
			if s.metaRemaining == 0 {
				s.checkMeta(l)
				s.untilMeta = s.metaint
			}
		case s.untilMeta == 0:
			s.metaRemaining = int(b[0]) * 16
			b = b[1:]
			if s.metaRemaining == 0 {
				s.untilMeta = s.metaint
			}
		default:
			take := min(len(b), s.untilMeta)
			stream += take
			b = b[take:]
			s.untilMeta -= take
		}
	}
	return stream
}

// checkMeta validates one metadata block. A block of only padding carries
// no update and is neither counted nor flagged.
func (s *icyState) checkMeta(l *listener) {
	m := bytes.TrimRight(s.meta, "\x00")
	s.meta = s.meta[:0]
	switch {
	case len(m) == 0:
	case bytes.Contains(m, []byte("StreamTitle=")):
		l.mount.metaOK.Add(1)
	default:
		l.mount.metaBad.Add(1)
		logf("%s: bad ICY metadata block %q", l.mount.Path, m)
	}
}

// sample measures every live listener once per tick and fails the ones that
// keep falling behind the mount median.
func (p *Pool) sample() {
	t := time.NewTicker(p.opts.Tick)
	defer t.Stop()
	prevTotal := map[*mountState]int64{}
	w := p.opts.Window
	for {
		select {
		case <-p.done:
			return
		case now := <-t.C:
			for _, m := range p.mounts {
				total := m.total.Load()
				st := TickStat{T: now, Mount: m.Path, Bps: float64(total-prevTotal[m]) / p.opts.Tick.Seconds()}
				prevTotal[m] = total
				var rates []float64
				var eligible []*listener
				m.mu.Lock()
				st.Live = len(m.live)
				for l := range m.live {
					l.hist[l.ticks%(w+1)] = l.bytes.Load()
					l.ticks++
					// Skip the first window: it holds the connection burst.
					if l.ticks <= 2*w {
						continue
					}
					delta := l.hist[(l.ticks-1)%(w+1)] - l.hist[l.ticks%(w+1)]
					rates = append(rates, float64(delta)/(float64(w)*p.opts.Tick.Seconds()))
					eligible = append(eligible, l)
				}
				m.mu.Unlock()
				if len(rates) > 0 {
					sorted := append([]float64(nil), rates...)
					sort.Float64s(sorted)
					st.MedianBps = sorted[len(sorted)/2]
					st.MinBps = sorted[0]
					floor := st.MedianBps * (1 - p.opts.LagTolerance)
					for i, l := range eligible {
						if rates[i] < floor {
							l.lagging++
							st.Lagging++
							if l.lagging >= p.opts.LagTicks && !l.lagFailed.Swap(true) {
								logf("%s: %s: %.0f B/s vs median %.0f B/s", m.Path, endLagging, rates[i], st.MedianBps)
								l.conn.Close()
							}
						} else {
							l.lagging = 0
						}
					}
				}
				p.mu.Lock()
				p.ticks = append(p.ticks, st)
				p.mu.Unlock()
			}
		}
	}
}

// MountSnapshot is what a mount's counters look like at one instant.
type MountSnapshot struct {
	Live     int              `json:"live"`
	Connects int64            `json:"connects"`
	Ends     map[string]int64 `json:"ends"`
	MetaOK   int64            `json:"meta_ok"`
	MetaBad  int64            `json:"meta_bad"`
	TTFBs    []time.Duration  `json:"-"`
}

func (p *Pool) Snapshot() map[string]MountSnapshot {
	out := map[string]MountSnapshot{}
	for _, m := range p.mounts {
		m.mu.Lock()
		s := MountSnapshot{Live: len(m.live), Connects: m.connects, Ends: map[string]int64{}, MetaOK: m.metaOK.Load(), MetaBad: m.metaBad.Load()}
		for k, v := range m.ends {
			s.Ends[k] = v
		}
		s.TTFBs = append([]time.Duration(nil), m.ttfbs...)
		m.mu.Unlock()
		out[m.Path] = s
	}
	return out
}

func (p *Pool) Ticks() []TickStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]TickStat(nil), p.ticks...)
}

var (
	logMu    sync.Mutex
	logLines []string
	logLimit = 500
)

// logf keeps the first few hundred listener-level events for the report.
func logf(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	if len(logLines) < logLimit {
		logLines = append(logLines, time.Now().Format("15:04:05.000")+" "+fmt.Sprintf(format, args...))
	}
}
