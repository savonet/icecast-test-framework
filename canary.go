package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

// Canary is one ffmpeg decode of a mount: a listener that proves the bytes
// are a playable stream from the point where it joined.
type Canary struct {
	Step     int           `json:"step"`
	Mount    string        `json:"mount"`
	Start    time.Time     `json:"start"`
	Duration time.Duration `json:"duration"`
	OK       bool          `json:"ok"`
	Error    string        `json:"error,omitempty"`
}

// With strict set, any decode error fails the canary, which catches a stream
// whose first bytes fall mid-frame. Without it only a stream ffmpeg cannot
// open or keep decoding fails.
func runCanary(ctx context.Context, url string, step int, mount string, dur time.Duration, strict bool) Canary {
	c := Canary{Step: step, Mount: mount, Start: time.Now()}
	ctx, cancel := context.WithTimeout(ctx, 3*dur+15*time.Second)
	defer cancel()
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if strict {
		args = append(args, "-xerror")
	}
	args = append(args, "-user_agent", "icetest-canary/0.1", "-i", url, "-t", dur.String(), "-f", "null", "-")
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	c.Duration = time.Since(c.Start)
	c.OK = err == nil
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 1000 {
			msg = msg[len(msg)-1000:]
		}
		c.Error = err.Error() + ": " + msg
	}
	return c
}
