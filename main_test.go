package main_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var bin string

const sampleRate = 48000

func silence(d time.Duration) []float64 {
	return make([]float64, int(d.Seconds()*sampleRate))
}

func sine(hz, amplitude float64, d time.Duration) []float64 {
	s := make([]float64, int(d.Seconds()*sampleRate))
	for i := range s {
		s[i] = amplitude * math.Sin(2*math.Pi*hz*float64(i)/sampleRate)
	}
	return s
}

// pcm encodes samples in -1..1 as 16-bit little-endian, the format govee music reads.
func pcm(samples []float64) []byte {
	b := make([]byte, 0, 2*len(samples))
	for _, s := range samples {
		b = binary.LittleEndian.AppendUint16(b, uint16(int16(s*32767)))
	}
	return b
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "govee-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin = filepath.Join(dir, "govee")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// All cases use --dry-run, which prints plaintext packets instead of
// talking to Bluetooth, so they run without any lamps nearby.
func TestCLI(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		stdin      []byte
		wantStdout string
		wantStderr string // substring; empty means stderr must be empty
		wantUsage  bool   // stdout must be the usage help instead of wantStdout
		wantCode   int
	}{
		{
			name: "on without lamps means all lamps",
			args: []string{"--dry-run", "on"},
			wantStdout: "" +
				"drawer 5C:E7:53:C8:3A:37 33 01 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 33\n" +
				"table 5C:E7:53:C7:2D:2F 33 01 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 33\n",
		},
		{
			name:       "off one lamp",
			args:       []string{"--dry-run", "off", "table"},
			wantStdout: "table 5C:E7:53:C7:2D:2F 33 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 32\n",
		},
		{
			name:       "color by name",
			args:       []string{"--dry-run", "color", "red", "drawer"},
			wantStdout: "drawer 5C:E7:53:C8:3A:37 33 05 0d ff 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 c4\n",
		},
		{
			name:       "color by hex with hash",
			args:       []string{"--dry-run", "color", "#FF8000", "table"},
			wantStdout: "table 5C:E7:53:C7:2D:2F 33 05 0d ff 80 00 00 00 00 00 00 00 00 00 00 00 00 00 00 44\n",
		},
		{
			name:       "brightness scales the color",
			args:       []string{"--dry-run", "color", "--brightness", "50", "red", "table"},
			wantStdout: "table 5C:E7:53:C7:2D:2F 33 05 0d 80 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 bb\n",
		},
		{
			name:       "brightness after the lamp name",
			args:       []string{"--dry-run", "color", "ff8000", "table", "--brightness", "25"},
			wantStdout: "table 5C:E7:53:C7:2D:2F 33 05 0d 40 20 00 00 00 00 00 00 00 00 00 00 00 00 00 00 5b\n",
		},
		{
			name:       "brightness 0 is rejected",
			args:       []string{"--dry-run", "color", "--brightness", "0", "red"},
			wantStderr: "govee: invalid brightness 0 (want 1 to 100; use off to turn lamps off)",
			wantCode:   1,
		},
		{
			name:       "brightness over 100 is rejected",
			args:       []string{"--dry-run", "color", "--brightness", "101", "red"},
			wantStderr: "govee: invalid brightness 101",
			wantCode:   1,
		},
		{
			name:       "lamp by lower-case MAC gets its name",
			args:       []string{"--dry-run", "color", "00ff00", "5c:e7:53:c7:2d:2f"},
			wantStdout: "table 5C:E7:53:C7:2D:2F 33 05 0d 00 ff 00 00 00 00 00 00 00 00 00 00 00 00 00 00 c4\n",
		},
		{
			name:       "unknown MAC is used as its own name",
			args:       []string{"--dry-run", "on", "AA:BB:CC:DD:EE:FF"},
			wantStdout: "AA:BB:CC:DD:EE:FF AA:BB:CC:DD:EE:FF 33 01 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 33\n",
		},
		{
			name: "duplicate lamps are sent once, in order",
			args: []string{"--dry-run", "off", "table", "table", "all"},
			wantStdout: "" +
				"table 5C:E7:53:C7:2D:2F 33 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 32\n" +
				"drawer 5C:E7:53:C8:3A:37 33 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 32\n",
		},
		{
			name:       "raw command gets a checksum",
			args:       []string{"--dry-run", "raw", "aa01", "table"},
			wantStdout: "table 5C:E7:53:C7:2D:2F aa 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 ab\n",
		},
		{
			name:       "unknown lamp",
			args:       []string{"--dry-run", "on", "kitchen"},
			wantStderr: `govee: unknown lamp "kitchen" (want drawer, table, all, or a MAC address)`,
			wantCode:   1,
		},
		{
			name:       "invalid color",
			args:       []string{"--dry-run", "color", "zz"},
			wantStderr: `govee: invalid color "zz"`,
			wantCode:   1,
		},
		{
			name:       "missing color",
			args:       []string{"--dry-run", "color"},
			wantStderr: "govee:",
			wantUsage:  true,
			wantCode:   1,
		},
		{
			name:       "music: silence is dim red, one line per 1/20 s",
			args:       []string{"--dry-run", "music", "--input", "-"},
			stdin:      pcm(silence(1 * time.Second)),
			wantStdout: strings.Repeat("080000\n", 20),
		},
		{
			name: "music: first bass note after silence is a bright color change",
			args: []string{"--dry-run", "music", "--input", "-", "--rate", "2"},
			// At 2 updates/s, each line covers 0.5 s of audio.
			stdin:      pcm(append(silence(500*time.Millisecond), sine(60, 0.5, 500*time.Millisecond)...)),
			wantStdout: "080000\n00ff4a\n",
		},
		{
			name:       "music: incomplete last frame is ignored",
			args:       []string{"--dry-run", "music", "--input", "-", "--rate", "2"},
			stdin:      pcm(silence(900 * time.Millisecond)),
			wantStdout: "080000\n",
		},
		{
			name:       "music rejects bad rate",
			args:       []string{"--dry-run", "music", "--input", "-", "--rate", "0"},
			wantStderr: "govee: invalid rate 0",
			wantCode:   1,
		},
		{
			name:       "bench rejects bad rates before connecting",
			args:       []string{"bench", "--rates", "1,fast"},
			wantStderr: `govee: invalid rate "fast" in "1,fast"`,
			wantCode:   1,
		},
		{
			name:       "raw command too long",
			args:       []string{"--dry-run", "raw", strings.Repeat("00", 20)},
			wantStderr: "govee: invalid raw command",
			wantCode:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), bin, tt.args...)
			cmd.Stdin = bytes.NewReader(tt.stdin)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr

			code := 0
			if err := cmd.Run(); err != nil {
				exitErr, ok := errors.AsType[*exec.ExitError](err)
				if !ok {
					t.Fatal(err)
				}
				code = exitErr.ExitCode()
			}

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %q)", code, tt.wantCode, stderr.String())
			}
			if tt.wantUsage {
				if !strings.Contains(stdout.String(), "USAGE:") {
					t.Errorf("stdout = %q, want usage help", stdout.String())
				}
			} else if got := stdout.String(); got != tt.wantStdout {
				t.Errorf("stdout:\ngot:  %q\nwant: %q", got, tt.wantStdout)
			}
			if tt.wantStderr == "" && stderr.Len() > 0 {
				t.Errorf("unexpected stderr: %q", stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
