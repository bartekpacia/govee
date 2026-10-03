package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urfave/cli/v3"
)

// minSecretLen is the shortest secret path segment accepted without a password.
// It is the length of crypto/rand.Text, which carries 128 bits of randomness.
const minSecretLen = 26

// radio serializes Bluetooth use. BlueZ misbehaves when two commands
// scan or connect at once, and a client may call tools in parallel.
var radio sync.Mutex

// serveMCP serves an MCP endpoint that controls the lamps.
// It returns when ctx is canceled (interrupt or SIGTERM).
func serveMCP(ctx context.Context, cmd *cli.Command) error {
	user := cmd.String("user")
	password := cmd.String("password")
	path := cmd.String("path")
	if path == "" || !strings.HasPrefix(path, "/") || strings.Contains(path, "?") {
		return fmt.Errorf("invalid path %q (want an absolute path like /mcp)", path)
	}
	if password != "" && user == "" {
		return errors.New("set a user with --user or GOVEE_USER")
	}
	// Without a password, the path itself is the secret: clients that
	// can't send headers (claude.ai custom connectors) can still connect.
	if _, secret, _ := strings.CutLast(path, "/"); password == "" && len(secret) < minSecretLen {
		return fmt.Errorf("set a password with --password or GOVEE_PASSWORD, "+
			"or make the last part of --path (or GOVEE_PATH) a secret of at least %d characters, e.g. --path /mcp-%s",
			minSecretLen, rand.Text())
	}

	server := newMCPServer(logger(cmd), cmd.Bool("dry-run"), cmd.Duration("timeout"))
	var handler http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		// Stateless covers both older clients and the 2026-07-28 protocol,
		// which the streamable transport only speaks when this is set.
		Stateless:    true,
		JSONResponse: true,
		// A reverse proxy on this machine connects over loopback but
		// forwards the public Host header. The SDK would reject that
		// as a DNS-rebinding attempt. The password is still required,
		// and the handler sets no CORS headers.
		DisableLocalhostProtection: true,
	})

	mux := http.NewServeMux()
	if password != "" {
		handler = requireBasicAuth(user, password, handler)
	}
	mux.Handle(path, handler)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", cmd.String("listen"))
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	fmt.Fprintf(cmd.Root().Writer, "listening on http://%s%s\n", ln.Addr(), path)

	select {
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(shut); err != nil {
			return err
		}
		<-errc
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func requireBasicAuth(user, password string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPassword, ok := r.BasicAuth()
		// Compare both sides even when the header is missing, so the
		// timing does not reveal which check failed.
		userOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(gotPassword), []byte(password)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="govee", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type lampInput struct {
	Lamps []string `json:"lamps,omitempty" jsonschema:"lamp names, \"all\", or BLE MAC addresses. Omit to use every known lamp."`
}

type colorInput struct {
	Color      string   `json:"color" jsonschema:"a color name (red, green, blue, orange, yellow, purple, pink, cyan, white) or RRGGBB hex, optionally prefixed with #"`
	Brightness *int     `json:"brightness,omitempty" jsonschema:"percent of full brightness, from 1 to 100. Omit for 100. Dims by scaling the color."`
	Lamps      []string `json:"lamps,omitempty" jsonschema:"lamp names, \"all\", or BLE MAC addresses. Omit to use every known lamp."`
}

type scanInput struct {
	Seconds int `json:"seconds,omitempty" jsonschema:"how many seconds to scan, from 1 to 30. Omit for 5."`
}

type mcpTools struct {
	logf    logFunc
	dry     bool
	timeout time.Duration
}

func newMCPServer(logf logFunc, dry bool, timeout time.Duration) *mcp.Server {
	t := &mcpTools{logf: logf, dry: dry, timeout: timeout}
	lamps := strings.Join(slices.Sorted(maps.Keys(knownLamps)), ", ")
	colors := strings.Join(slices.Sorted(maps.Keys(namedColors)), ", ")
	server := mcp.NewServer(&mcp.Implementation{Name: "govee", Version: "0"}, &mcp.ServerOptions{
		Instructions: fmt.Sprintf(
			"Controls Govee H6006 bulbs over Bluetooth. Known lamps: %s. "+
				"Color names: %s. Hex colors such as ff8000 also work. "+
				"Brightness is a percent from 1 to 100 and dims by scaling the color. "+
				"Omitting the lamps argument changes every known lamp. "+
				"A command takes a few seconds, because each one connects over Bluetooth. "+
				"Only one Bluetooth client can be connected to a bulb; the Govee phone app has to be closed. "+
				"list_lamps does not use the radio. scan_lamps does, and reports bulbs that are nearby.",
			lamps, colors,
		),
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_lamps",
		Description: "List the known lamps and their Bluetooth addresses. Does not use the radio or change any lamp.",
	}, t.listLamps)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "scan_lamps",
		Description: "Scan for nearby Govee bulbs and report signal strength. Use this when a lamp does not respond.",
	}, t.scanLamps)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "turn_on",
		Description: "Turn lamps on. Known lamps: " + lamps + ". Omit lamps to turn every lamp on.",
	}, t.turnOn)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "turn_off",
		Description: "Turn lamps off. Known lamps: " + lamps + ". Omit lamps to turn every lamp off.",
	}, t.turnOff)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "set_color",
		Description: "Set the color and brightness of lamps. Color names: " + colors + ". Or RRGGBB hex.",
	}, t.setColor)
	return server
}

func (t *mcpTools) listLamps(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(knownLamps)) {
		fmt.Fprintf(&b, "%s %s\n", name, knownLamps[name])
	}
	return textResult(b.String()), nil, nil
}

func (t *mcpTools) scanLamps(ctx context.Context, _ *mcp.CallToolRequest, in scanInput) (*mcp.CallToolResult, any, error) {
	seconds := in.Seconds
	if seconds == 0 {
		seconds = 5
	}
	if seconds < 1 || seconds > 30 {
		return nil, nil, fmt.Errorf("invalid seconds %d (want 1 to 30)", seconds)
	}
	if t.dry {
		return textResult("dry-run: not scanning\n"), nil, nil
	}
	radio.Lock()
	defer radio.Unlock()
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	devices, err := discover(ctx, time.Duration(seconds)*time.Second)
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ADDRESS\tRSSI\tNAME\tLAMP\n")
	for _, d := range devices {
		name := lampName(d.addr)
		if name == d.addr {
			name = "-"
		}
		fmt.Fprintf(&b, "%s\t%d\t%s\t%s\n", d.addr, d.rssi, d.name, name)
	}
	return textResult(b.String()), nil, nil
}

func (t *mcpTools) turnOn(ctx context.Context, _ *mcp.CallToolRequest, in lampInput) (*mcp.CallToolResult, any, error) {
	return t.send(ctx, in.Lamps, powerPacket(true))
}

func (t *mcpTools) turnOff(ctx context.Context, _ *mcp.CallToolRequest, in lampInput) (*mcp.CallToolResult, any, error) {
	return t.send(ctx, in.Lamps, powerPacket(false))
}

func (t *mcpTools) setColor(ctx context.Context, _ *mcp.CallToolRequest, in colorInput) (*mcp.CallToolResult, any, error) {
	percent := 100
	if in.Brightness != nil {
		percent = *in.Brightness
	}
	payload, err := colorPayload(in.Color, percent)
	if err != nil {
		return nil, nil, err
	}
	return t.send(ctx, in.Lamps, payload)
}

// send delivers payload to the named lamps. With no names, it uses every known lamp.
// In dry-run mode it returns the packets that would be sent.
func (t *mcpTools) send(ctx context.Context, names []string, payload []byte) (*mcp.CallToolResult, any, error) {
	lamps, err := resolveLamps(names)
	if err != nil {
		return nil, nil, err
	}
	if t.dry {
		return textResult(formatPackets(lamps, payload)), nil, nil
	}
	radio.Lock()
	defer radio.Unlock()
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	results, err := sendAll(ctx, lamps, payload, t.logf)
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	failed := false
	for i, l := range lamps {
		if results[i] != nil {
			failed = true
			fmt.Fprintf(&b, "%s: %s\n", l.name, results[i])
			continue
		}
		fmt.Fprintf(&b, "%s: ok\n", l.name)
	}
	res := textResult(b.String())
	res.IsError = failed
	return res, nil, nil
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}
