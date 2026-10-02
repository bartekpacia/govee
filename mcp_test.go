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

func TestMCP(t *testing.T) {
	url := startMCP(t, []string{"GOVEE_USER=from-env", "GOVEE_PASSWORD=from-env"},
		"--dry-run", "mcp", "--listen", "127.0.0.1:0", "--user", "ada", "--password", "secret")

	t.Run("missing auth", func(t *testing.T) {
		status, www := mcpStatus(t, url, "", "")
		if status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
		if !strings.Contains(www, "Basic") {
			t.Errorf("WWW-Authenticate = %q, want a Basic challenge", www)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		status, _ := mcpStatus(t, url, "ada", "nope")
		if status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("flag overrides environment", func(t *testing.T) {
		// The process was started with GOVEE_PASSWORD=from-env and --password secret.
		status, _ := mcpStatus(t, url, "from-env", "from-env")
		if status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 (the flag should win over the environment)", status)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := connectMCP(t, ctx, url, "ada", "secret")

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

	t.Run("public host on loopback", func(t *testing.T) {
		// Caddy on this machine dials 127.0.0.1 and forwards Host: lights.pacia.tech.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "lights.pacia.tech"
		req.SetBasicAuth("ada", "secret")
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

func TestMCPPasswordFromEnv(t *testing.T) {
	url := startMCP(t, []string{"GOVEE_USER=ada", "GOVEE_PASSWORD=secret"},
		"--dry-run", "mcp", "--listen", "127.0.0.1:0")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := connectMCP(t, ctx, url, "ada", "secret")
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

func TestMCPRequiresPassword(t *testing.T) {
	cmd := exec.Command(bin, "mcp", "--listen", "127.0.0.1:0")
	cmd.Env = withoutEnv(os.Environ(), "GOVEE_PASSWORD", "GOVEE_USER")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("mcp started without a password")
	}
	if !strings.Contains(stderr.String(), "GOVEE_PASSWORD") {
		t.Errorf("stderr = %q, want it to mention GOVEE_PASSWORD", stderr.String())
	}
}

func startMCP(t *testing.T, extraEnv []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(withoutEnv(os.Environ(), "GOVEE_PASSWORD", "GOVEE_USER"), extraEnv...)
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

func connectMCP(t *testing.T, ctx context.Context, url, user, password string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: url,
		HTTPClient: &http.Client{Transport: basicAuthTransport{
			base: http.DefaultTransport,
			user: user,
			pass: password,
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

type basicAuthTransport struct {
	base http.RoundTripper
	user string
	pass string
}

func (b basicAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.SetBasicAuth(b.user, b.pass)
	return b.base.RoundTrip(r)
}

func mcpStatus(t *testing.T, url, user, password string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if user != "" || password != "" {
		req.SetBasicAuth(user, password)
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
