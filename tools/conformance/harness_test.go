package conformance

import (
	"bytes"
	"context"
	sdkjson "encoding/json" //nolint:depguard // the SDK's types are coded with the SDK's encoder
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

// binary is the acpharness built from this checkout, once for all tests.
var binary string

// envBinary names an acpharness binary to test in place of one built from
// this checkout, so that the binary of another branch can be checked.
// envWire prints both directions of every conversation with -v.
const (
	envBinary = "ACPHARNESS_CONFORMANCE_BINARY"
	envWire   = "ACPHARNESS_CONFORMANCE_WIRE"
)

func TestMain(m *testing.M) {
	if binary = os.Getenv(envBinary); binary != "" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "acpharness-conformance")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "acpharness")
	// Built from the repository root: cmd/acpharness is in the root module,
	// and go build refuses a package outside the module it runs in.
	build := exec.Command("go", "build", "-o", binary, "./cmd/acpharness")
	build.Dir = filepath.Join("..", "..")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build acpharness:", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

// wire keeps every byte that crossed one direction of the connection, so
// that a finding can be reported with the exact bytes.
type wire struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *wire) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// lines returns the complete lines seen so far.
func (w *wire) lines() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	text := w.buf.String()
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		return strings.Split(text[:i], "\n")
	}
	return nil
}

// sdkLog collects what the SDK logs at error level. The SDK reports a
// message it cannot parse and a notification it cannot decode only there.
type sdkLog struct {
	mu      sync.Mutex
	records []string
}

func (l *sdkLog) Enabled(_ context.Context, level slog.Level) bool { return level >= slog.LevelError }

func (l *sdkLog) Handle(_ context.Context, r slog.Record) error {
	text := r.Message
	r.Attrs(func(a slog.Attr) bool {
		text += fmt.Sprintf(" %s=%v", a.Key, a.Value)
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, text)
	return nil
}

func (l *sdkLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *sdkLog) WithGroup(string) slog.Handler      { return l }

// client is the SDK's Client for a test: it keeps the session updates and
// refuses everything the agent could ask of it, since it advertises none.
type client struct {
	mu      sync.Mutex
	updates []acp.SessionNotification
}

func (c *client) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, n)
	return nil
}

// take returns the updates received since the last call.
func (c *client) take() []acp.SessionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.updates
	c.updates = nil
	return out
}

func (c *client) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsReadTextFile)
}

func (c *client) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsWriteTextFile)
}

func (c *client) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{}, acp.NewMethodNotFound(acp.ClientMethodSessionRequestPermission)
}

func (c *client) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalCreate)
}

func (c *client) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalKill)
}

func (c *client) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalOutput)
}

func (c *client) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalRelease)
}

func (c *client) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalWaitForExit)
}

// agent is one running "acpharness serve" and what crossed its pipes.
type agent struct {
	t *testing.T
	// sent is what the client wrote and received what the agent wrote.
	sent, received *wire
	stdin          io.WriteCloser
	stdout         io.Reader
}

// start runs the built binary with a config file of its own and no other
// config layer, and stops it when the test ends.
func start(t *testing.T) *agent {
	t.Helper()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.toml")
	// Only errors on stderr, so that a failure shows what the agent said.
	require.NoError(t, os.WriteFile(configFile, []byte("[telemetry.logs.stderr]\nlevel = \"error\"\n"), 0o600))

	cmd := exec.Command(binary, "--config", configFile, "serve")
	// No user or system config, and a state directory of the test's own.
	cmd.Env = []string{
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"),
		"XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"HOME=" + dir,
	}
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	a := &agent{t: t, sent: &wire{}, received: &wire{}}
	a.stdin = stdin
	a.stdout = io.TeeReader(stdout, a.received)
	t.Cleanup(func() {
		// The client closing standard input ends the agent without an error.
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("acpharness serve ended with %v\nstderr:\n%s", err, stderr.String())
			}
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			t.Errorf("acpharness serve did not end after its standard input closed\nstderr:\n%s", stderr.String())
		}
		if t.Failed() || os.Getenv(envWire) != "" {
			t.Logf("client sent:\n%s", strings.Join(a.sent.lines(), "\n"))
			t.Logf("agent sent:\n%s", strings.Join(a.received.lines(), "\n"))
		}
	})
	return a
}

// writer is where the SDK writes: the agent's standard input, copied.
func (a *agent) writer() io.Writer { return io.MultiWriter(a.stdin, a.sent) }

// connect attaches the SDK's client side. A message the SDK cannot parse or
// a notification it cannot decode fails the test, with the SDK's own words.
func (a *agent) connect() (*acp.ClientSideConnection, *client) {
	a.t.Helper()
	c := &client{}
	log := &sdkLog{}
	conn := acp.NewClientSideConnection(c, a.writer(), a.stdout)
	conn.SetLogger(slog.New(log))
	a.t.Cleanup(func() {
		log.mu.Lock()
		defer log.mu.Unlock()
		for _, record := range log.records {
			a.t.Errorf("the SDK could not handle something the agent sent: %s", record)
		}
	})
	return conn, c
}

// message is one JSON-RPC line the agent wrote.
type message struct {
	ID     jsontext.Value `json:"id"`
	Method string         `json:"method"`
	Params jsontext.Value `json:"params"`
	Result jsontext.Value `json:"result"`
	Error  jsontext.Value `json:"error"`
}

// messages decodes every line the agent has written.
func (a *agent) messages() []message {
	a.t.Helper()
	var out []message
	for _, line := range a.received.lines() {
		var m message
		require.NoError(a.t, json.Unmarshal([]byte(line), &m), "the agent wrote a line that is not JSON: %s", line)
		out = append(out, m)
	}
	return out
}

// lastResult is the result of the newest response the agent wrote, which is
// the answer to the call that just returned.
func (a *agent) lastResult() jsontext.Value {
	a.t.Helper()
	msgs := a.messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Method == "" && len(msgs[i].Result) > 0 {
			return msgs[i].Result
		}
	}
	a.t.Fatal("the agent wrote no result")
	return nil
}

// notDecoded lists the members of raw that decoded does not hold once it is
// encoded again: what the SDK dropped when it read the agent's bytes. A
// list's elements share one path.
func notDecoded(t *testing.T, raw jsontext.Value, decoded any) []string {
	t.Helper()
	again, err := sdkjson.Marshal(decoded)
	require.NoError(t, err)
	var sent, kept any
	require.NoError(t, json.Unmarshal(raw, &sent))
	require.NoError(t, json.Unmarshal(again, &kept))
	found := map[string]bool{}
	missing("", sent, kept, found)
	out := make([]string, 0, len(found))
	for path := range found {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func missing(path string, sent, kept any, found map[string]bool) {
	switch sent := sent.(type) {
	case map[string]any:
		keptMap, _ := kept.(map[string]any)
		for key, value := range sent {
			at := strings.TrimPrefix(path+"."+key, ".")
			keptValue, ok := keptMap[key]
			if !ok {
				found[at] = true
				continue
			}
			missing(at, value, keptValue, found)
		}
	case []any:
		keptList, _ := kept.([]any)
		for i, value := range sent {
			var keptValue any
			if i < len(keptList) {
				keptValue = keptList[i]
			}
			missing(path+"[]", value, keptValue, found)
		}
	}
}

// normalized returns the lines the agent wrote with what differs between
// runs replaced: session ids, the build's version, and the text of a decode
// error, which encoding/json/v2 changes between runs on purpose. Members
// are in sorted order, since their order means nothing to a client.
func (a *agent) normalized() []string {
	a.t.Helper()
	var out []string
	for _, line := range a.received.lines() {
		var v any
		require.NoError(a.t, json.Unmarshal([]byte(line), &v), "line: %s", line)
		normalize(nil, v)
		again, err := json.Marshal(v, json.Deterministic(true))
		require.NoError(a.t, err)
		out = append(out, string(again))
	}
	return out
}

func normalize(path []string, v any) {
	switch v := v.(type) {
	case map[string]any:
		for key, value := range v {
			at := make([]string, 0, len(path)+1)
			at = append(append(at, path...), key)
			text, isText := value.(string)
			switch {
			case !isText:
				normalize(at, value)
			case key == "sessionId" && text != unknownSession:
				v[key] = "<session>"
			case strings.HasSuffix(strings.Join(at, "."), "agentInfo.version"):
				v[key] = "<version>"
			case strings.HasSuffix(strings.Join(at, "."), "error.data.error"):
				v[key] = "<text>"
			}
		}
	case []any:
		for _, value := range v {
			normalize(path, value)
		}
	}
}
