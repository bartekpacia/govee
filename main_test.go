package main_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var bin string

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
			name:       "raw command too long",
			args:       []string{"--dry-run", "raw", strings.Repeat("00", 20)},
			wantStderr: "govee: invalid raw command",
			wantCode:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), bin, tt.args...)
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
