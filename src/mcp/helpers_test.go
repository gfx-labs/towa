package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/httpclient"
)

// serverModeEnv makes the test binary run as an MCP server on its stdin and
// stdout. That is the stdio server the tests start: a real process speaking
// the SDK's server side of the protocol.
const serverModeEnv = "TOWA_MCP_TEST_SERVER"

// Modes of the test server.
const (
	// modeServe answers until its stdin closes, then exits.
	modeServe = "serve"
	// modeLinger answers until its stdin closes, then stays.
	modeLinger = "linger"
	// modeMute never answers.
	modeMute = "mute"
	// modeVanish answers until its vanish tool is called, then exits
	// without answering that call.
	modeVanish = "vanish"
)

func TestMain(m *testing.M) {
	switch mode := os.Getenv(serverModeEnv); mode {
	case "":
		os.Exit(m.Run())
	case modeMute:
		select {}
	case modeServe, modeLinger, modeVanish:
		server := newTestServer()
		if mode == modeVanish {
			sdk.AddTool(server, &sdk.Tool{
				Name:        "vanish",
				Description: "Take the call, write the file named by path, and exit without answering.",
			}, func(_ context.Context, _ *sdk.CallToolRequest, in vanishInput) (*sdk.CallToolResult, any, error) {
				// The file is the effect a caller cannot learn about.
				if err := os.WriteFile(in.Path, []byte("ran"), 0o600); err != nil {
					return nil, nil, err
				}
				os.Exit(0)
				return nil, nil, nil
			})
		}
		err := server.Run(context.Background(), &sdk.StdioTransport{})
		if mode == modeLinger {
			select {}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown %s=%q\n", serverModeEnv, mode)
		os.Exit(2)
	}
}

type echoInput struct {
	Text string `json:"text" jsonschema:"the text to return"`
}

type noInput struct{}

type vanishInput struct {
	Path string `json:"path"`
}

type pidsOutput struct {
	PID   int `json:"pid"`
	Child int `json:"child"`
}

// onePixelPNG is a valid 1x1 PNG.
var onePixelPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

// newTestServer is the server every transport test talks to.
func newTestServer() *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "towa-test-server", Version: "1"}, nil)
	sdk.AddTool(server, &sdk.Tool{
		Name:        "echo",
		Description: "Return the text it is given.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *sdk.CallToolRequest, in echoInput) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: in.Text}}}, nil, nil
	})
	sdk.AddTool(server, &sdk.Tool{
		Name:        "submit_result",
		Description: "Hand in the result of the task.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in echoInput) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "accepted: " + in.Text}}}, nil, nil
	})
	sdk.AddTool(server, &sdk.Tool{
		Name:        "fail",
		Description: "Always report an error.",
	}, func(context.Context, *sdk.CallToolRequest, noInput) (*sdk.CallToolResult, any, error) {
		return nil, nil, errors.New("the disk is full")
	})
	sdk.AddTool(server, &sdk.Tool{
		Name:        "picture",
		Description: "Return a caption and a one pixel image.",
	}, func(context.Context, *sdk.CallToolRequest, noInput) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.TextContent{Text: "one pixel"},
			&sdk.ImageContent{MIMEType: "image/png", Data: onePixelPNG},
		}}, nil, nil
	})
	sdk.AddTool(server, &sdk.Tool{
		Name:        "pids",
		Description: "Start a child process that outlives the server and report both process ids.",
	}, func(context.Context, *sdk.CallToolRequest, noInput) (*sdk.CallToolResult, pidsOutput, error) {
		// The child keeps the server's stderr, as a server started through
		// a wrapper script does.
		child := exec.Command("sleep", "600")
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			return nil, pidsOutput{}, err
		}
		return nil, pidsOutput{PID: os.Getpid(), Child: child.Process.Pid}, nil
	})
	return server
}

// stdioServer is the test binary run as a server.
func stdioServer(t *testing.T, name, mode string) Server {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	return Server{Name: name, Transport: config.MCPTransportStdio, Command: self, Env: []string{serverModeEnv + "=" + mode}}
}

// httpServer serves newTestServer over one of the SDK's HTTP handlers. It
// counts the requests it gets and keeps the last value of X-Test-Key.
type httpServer struct {
	URL      string
	requests atomic.Int64
	lastKey  atomic.Value
}

func startHTTPServer(t *testing.T, transport string, wrap func(http.Handler) http.Handler) *httpServer {
	t.Helper()
	server := newTestServer()
	getServer := func(*http.Request) *sdk.Server { return server }
	var handler http.Handler
	switch transport {
	case config.MCPTransportStreamableHTTP:
		handler = sdk.NewStreamableHTTPHandler(getServer, nil)
	case config.MCPTransportSSE:
		handler = sdk.NewSSEHandler(getServer, nil)
	default:
		t.Fatalf("no HTTP handler for transport %q", transport)
	}
	if wrap != nil {
		handler = wrap(handler)
	}
	h := &httpServer{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		if key := r.Header.Get("X-Test-Key"); key != "" {
			h.lastKey.Store(key)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	h.URL = ts.URL
	return h
}

// countingClient is a client from src/httpclient that counts its requests.
type countingClient struct {
	Client *http.Client
	// base owns the connections. Client wraps its transport, and a wrapped
	// transport hides CloseIdleConnections.
	base     *http.Client
	requests atomic.Int64
}

func newCountingClient() *countingClient {
	c := &countingClient{base: httpclient.New()}
	client, err := wrapClient(c.base, func(base http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			c.requests.Add(1)
			return base.RoundTrip(r)
		})
	})
	if err != nil {
		panic(err)
	}
	c.Client = client
	return c
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// defaultConfig is the mcp section with its real defaults.
func defaultConfig(t *testing.T) config.MCP {
	t.Helper()
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	return loaded.Config.MCP
}

// newTestManager returns a manager that is closed when the test ends.
func newTestManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	if opts.Config.ConnectTimeoutSeconds == 0 {
		opts.Config = defaultConfig(t)
	}
	opts.ClientName, opts.ClientVersion = "towa-test", "0"
	m, err := NewManager(opts)
	require.NoError(t, err)
	// A server that lingers is not given the default five seconds.
	m.terminateGrace = 200 * time.Millisecond
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	return m
}
