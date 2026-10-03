package main_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// token is long enough to be accepted (at least 26 characters).
const token = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"

func TestMCP(t *testing.T) {
	url := startMCP(t, []string{"GOVEE_TOKEN=from-env-0123456789abcdefghij"},
		"--dry-run", "mcp", "--listen", "127.0.0.1:0", "--token", token)

	t.Run("missing token", func(t *testing.T) {
		status, www := mcpStatus(t, url, "")
		if status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
		if !strings.HasPrefix(www, "Bearer") {
			t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", www)
		}
	})
	t.Run("wrong token", func(t *testing.T) {
		if status, _ := mcpStatus(t, url, "Bearer nope"); status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})
	t.Run("right token with the wrong scheme", func(t *testing.T) {
		if status, _ := mcpStatus(t, url, "Basic "+token); status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})
	t.Run("scheme is case-insensitive", func(t *testing.T) {
		if status, _ := mcpStatus(t, url, "bearer "+token); status != http.StatusOK {
			t.Errorf("status = %d, want 200", status)
		}
	})
	t.Run("flag overrides environment", func(t *testing.T) {
		// The process was started with both GOVEE_TOKEN and --token.
		if status, _ := mcpStatus(t, url, "Bearer from-env-0123456789abcdefghij"); status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 (the flag should win over the environment)", status)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := connectMCP(t, ctx, url, token)

	lt, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range lt.Tools {
		got[tool.Name] = true
	}
	for _, name := range []string{"list_lamps", "scan_lamps", "turn_on", "turn_off", "set_color"} {
		if !got[name] {
			t.Errorf("missing tool %s (have %v)", name, got)
		}
	}

	assertTool := func(t *testing.T, name string, args map[string]any, want string) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("tool error: %s", toolText(res))
		}
		if got := toolText(res); got != want {
			t.Errorf("result:\ngot:  %q\nwant: %q", got, want)
		}
	}

	t.Run("list lamps", func(t *testing.T) {
		assertTool(t, "list_lamps", map[string]any{},
			"drawer 5C:E7:53:C8:3A:37\ntable 5C:E7:53:C7:2D:2F\n")
	})
	t.Run("turn on every lamp", func(t *testing.T) {
		assertTool(t, "turn_on", map[string]any{},
			"drawer 5C:E7:53:C8:3A:37 33 01 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 33\n"+
				"table 5C:E7:53:C7:2D:2F 33 01 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 33\n")
	})
	t.Run("color by name", func(t *testing.T) {
		assertTool(t, "set_color", map[string]any{"color": "red", "lamps": []string{"drawer"}},
			"drawer 5C:E7:53:C8:3A:37 33 05 0d ff 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 c4\n")
	})
	t.Run("brightness scales the color", func(t *testing.T) {
		assertTool(t, "set_color", map[string]any{"color": "red", "brightness": 50, "lamps": []string{"table"}},
			"table 5C:E7:53:C7:2D:2F 33 05 0d 80 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 bb\n")
	})
	t.Run("duplicate lamps are sent once", func(t *testing.T) {
		assertTool(t, "turn_off", map[string]any{"lamps": []string{"table", "table", "all"}},
			"table 5C:E7:53:C7:2D:2F 33 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 32\n"+
				"drawer 5C:E7:53:C8:3A:37 33 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 32\n")
	})
	t.Run("scan does not use bluetooth in a dry run", func(t *testing.T) {
		assertTool(t, "scan_lamps", map[string]any{}, "dry-run: not scanning\n")
	})

	assertToolError := func(t *testing.T, name string, args map[string]any, want string) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Fatalf("got success %q, want a tool error containing %q", toolText(res), want)
		}
		if !strings.Contains(toolText(res), want) {
			t.Errorf("tool error %q, want it to contain %q", toolText(res), want)
		}
	}
	t.Run("unknown lamp", func(t *testing.T) {
		assertToolError(t, "turn_on", map[string]any{"lamps": []string{"kitchen"}}, `unknown lamp "kitchen"`)
	})
	t.Run("brightness 0", func(t *testing.T) {
		assertToolError(t, "set_color", map[string]any{"color": "red", "brightness": 0}, "invalid brightness 0")
	})
	t.Run("bad scan length", func(t *testing.T) {
		assertToolError(t, "scan_lamps", map[string]any{"seconds": 99}, "invalid seconds 99")
	})

	t.Run("public host behind a reverse proxy", func(t *testing.T) {
		// nginx dials the listen address and forwards Host: govee.pacia.tech.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "govee.pacia.tech"
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "list_lamps") {
			t.Fatalf("body = %s, want it to list tools", body)
		}
	})
}

func TestMCPTokenFromEnv(t *testing.T) {
	url := startMCP(t, []string{"GOVEE_TOKEN=" + token}, "--dry-run", "mcp", "--listen", "127.0.0.1:0")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := connectMCP(t, ctx, url, token)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_lamps"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatal(toolText(res))
	}
	if !strings.Contains(toolText(res), "table") {
		t.Errorf("result = %q, want it to mention table", toolText(res))
	}
}

func TestMCPRequiresToken(t *testing.T) {
	for name, args := range map[string][]string{
		"missing": {"mcp", "--listen", "127.0.0.1:0"},
		"short":   {"mcp", "--listen", "127.0.0.1:0", "--token", "short"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(bin, args...)
			cmd.Env = withoutEnv(os.Environ(), "GOVEE_TOKEN")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err == nil {
				t.Fatal("mcp started without a valid token")
			}
			if !strings.Contains(stderr.String(), "at least 26 characters with --token or GOVEE_TOKEN") {
				t.Errorf("stderr = %q, want it to explain the token", stderr.String())
			}
		})
	}
}

func startMCP(t *testing.T, extraEnv []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(withoutEnv(os.Environ(), "GOVEE_TOKEN"), extraEnv...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	buf := make([]byte, 256)
	n, err := stdout.Read(buf)
	line := string(buf[:n])
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if err != nil || !strings.HasPrefix(line, "listening on ") {
		t.Fatalf("server did not start (%v): %q", err, line)
	}
	return strings.TrimPrefix(line, "listening on ")
}

func connectMCP(t *testing.T, ctx context.Context, url, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: &http.Client{Transport: bearerTransport{base: http.DefaultTransport, token: token}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// mcpStatus sends a ping with the given Authorization header (none if empty)
// and returns the status code and the WWW-Authenticate header.
func mcpStatus(t *testing.T, url, authorization string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate")
}

func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func withoutEnv(env []string, keys ...string) []string {
	skip := make(map[string]bool, len(keys))
	for _, k := range keys {
		skip[k] = true
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if !skip[k] {
			out = append(out, e)
		}
	}
	return out
}
