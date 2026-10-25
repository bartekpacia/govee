//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// recordSystem records the default output's monitor with parec.
// That monitor is PulseAudio's copy of everything being played.
func recordSystem(ctx context.Context) (io.Reader, func(), error) {
	rec := exec.CommandContext(ctx, "parec", "-d", "@DEFAULT_MONITOR@",
		"--raw", "--format=s16le", fmt.Sprintf("--rate=%d", sampleRate), "--channels=1", "--latency-msec=20")
	rec.Stderr = os.Stderr
	out, err := rec.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := rec.Start(); err != nil {
		return nil, nil, fmt.Errorf("start parec: %w", err)
	}
	return out, func() { rec.Process.Kill(); rec.Wait() }, nil
}
