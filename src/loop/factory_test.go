package loop_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/acptest"
	"github.com/gfx-labs/acpharness/src/httpclient"
	"github.com/gfx-labs/acpharness/src/provider/providertest"
	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/store/dir"
	"github.com/gfx-labs/acpharness/src/transcript"
)

const recordFactoryWrite = "ACPHARNESS_RECORD_FACTORY_WRITE"

// recordFactoryUpstream is the Chat Completions server a recording is made
// from, such as http://127.0.0.1:1234. It must serve factoryModel.
const recordFactoryUpstream = "ACPHARNESS_FACTORY_WRITE_UPSTREAM"

// factoryModel is the model the recording was made with, the one tests are
// recorded from. A 3b model wrote the wrong content or no file in three of
// five recordings.
const factoryModel = "openai/gpt-oss-20b"

type processStderr struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *processStderr) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *processStderr) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// TestFactoryWriteProcess uses the built executable, actual runtime factory,
// core tools and durable store. The relay has exactly two slots, not a
// scripted model. Its replay only substitutes the test-owned working root.
func TestFactoryWriteProcess(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repo := filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	fixtures := filepath.Join(filepath.Dir(source), "testdata")
	recordRoot := os.Getenv(recordFactoryWrite)
	recording := recordRoot != ""
	if recording {
		require.True(t, filepath.IsAbs(recordRoot), "recording output must be an absolute staging directory")
		fixtures = recordRoot
	}
	paths := []string{filepath.Join(fixtures, "factory_write_1.json"), filepath.Join(fixtures, "factory_write_2.json")}
	home := t.TempDir()
	cwd := filepath.Join(home, "work")
	require.NoError(t, os.Mkdir(cwd, 0o700))
	var handlers []http.Handler
	var recorders []*providertest.Recorder
	previousRoot := cwd
	var upstream *url.URL
	if recording {
		require.NoError(t, os.MkdirAll(fixtures, 0o700))
		var err error
		upstream, err = url.Parse(os.Getenv(recordFactoryUpstream))
		require.NoError(t, err)
		require.NotEmpty(t, upstream.Host, "%s must be the server's address", recordFactoryUpstream)
		client := httpclient.New()
		for _, path := range paths {
			// The server's address says whose machine recorded.
			recorders = append(recorders, &providertest.Recorder{Next: client.Transport, Path: path, Secrets: []string{upstream.Host}})
		}
	} else {
		ex, err := providertest.Load(paths[0])
		require.NoError(t, err)
		// The prompt's working directory section names the recorded root.
		match := regexp.MustCompile(`Working directory: ([^"\\\s]+)`).FindSubmatch(ex.Request.Body)
		require.NotEmpty(t, match, "recorded prompt must name the working directory")
		previousRoot = string(match[1])
		for _, path := range paths {
			handlers = append(handlers, providertest.Replay(t, path, providertest.IgnoreHeader("User-Agent")).Config.Handler)
		}
	}
	var requests atomic.Int64
	t.Cleanup(func() { t.Logf("actual main requests: %d", requests.Load()) })
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(requests.Add(1)) - 1
		if n >= len(paths) {
			http.Error(w, "factory recording permits only two main requests", http.StatusBadGateway)
			t.Errorf("unexpected main request %d", n+1)
			return
		}
		if !recording {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				http.Error(w, "read request", http.StatusInternalServerError)
				return
			}
			req := r.Clone(r.Context())
			req.Body = io.NopCloser(bytes.NewReader(bytes.ReplaceAll(body, []byte(cwd), []byte(previousRoot))))
			replay := httptest.NewRecorder()
			handlers[n].ServeHTTP(replay, req)
			for name, values := range replay.Header() {
				w.Header()[name] = values
			}
			w.WriteHeader(replay.Code)
			_, err = w.Write(bytes.ReplaceAll(replay.Body.Bytes(), []byte(previousRoot), []byte(cwd)))
			if err != nil {
				t.Error(err)
			}
			return
		}
		target := upstream.ResolveReference(&url.URL{Path: r.URL.Path, RawQuery: r.URL.RawQuery})
		req := r.Clone(r.Context())
		req.URL = target
		req.Host = target.Host
		req.RequestURI = ""
		response, err := recorders[n].RoundTrip(req)
		if err != nil {
			t.Error(err)
			http.Error(w, "local upstream request", http.StatusBadGateway)
			return
		}
		defer func() {
			if err := response.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
		for name, values := range response.Header {
			if name != "Content-Length" && name != "Transfer-Encoding" {
				w.Header()[name] = values
			}
		}
		w.WriteHeader(response.StatusCode)
		if _, err := io.Copy(w, response.Body); err != nil {
			t.Error(err)
		}
	}))
	defer relay.Close()
	configuration := fmt.Sprintf(`providers:
  - id: local
    kind: chat
    base_url: %q
# The server takes no key. The recording holds no Authorization header, so
# the replay fails if one is sent.
accounts:
  - id: local
    provider: local
    kind: none
models: [%q]
model_context_window: 131072
model_max_output_tokens: 1024
store:
  root: %q
  compression: none
context:
  default_agents: false
  skills_enabled: false
  default_skills: false
compaction:
  mode: "off"
conversation_search:
  enabled: false
webfetch:
  enabled: false
websearch:
  enabled: false
tools:
  batch:
    enabled: false
  files:
    confine: true
subagents:
  max_depth: 0
limits:
  max_tool_calls: 1
  max_wall: 60s
  retry:
    max_retries: 0
telemetry:
  logs:
    stderr:
      level: error
`, relay.URL+"/v1", "local:local/"+factoryModel, filepath.Join(home, "store"))
	cfgDir := filepath.Join(home, "config", "acpharness")
	require.NoError(t, os.MkdirAll(cfgDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(configuration), 0o600))
	binary := filepath.Join(t.TempDir(), "acph")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./cmd/acph")
	build.Dir = repo
	output, err := build.CombinedOutput()
	require.NoError(t, err, "build: %s", output)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	wire, stdin, stdout := acptest.New(t)
	stopClose := context.AfterFunc(ctx, wire.Close)
	defer stopClose()
	var stderr processStderr
	command := exec.CommandContext(ctx, binary)
	command.Dir = cwd
	command.Env = []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "config"), "XDG_STATE_HOME=" + filepath.Join(home, "state"), "PATH=" + os.Getenv("PATH")}
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, &stderr
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		cancel()
		wire.Close()
		if command.ProcessState == nil {
			_ = command.Wait()
		}
	})
	send := func(id int, method string, params any) {
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		require.NoError(t, err)
		wire.Send(string(raw))
	}
	send(0, "initialize", map[string]any{"protocolVersion": 1})
	send(1, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	responses, _ := wire.Await("0", "1")
	require.Nil(t, responses["0"].Error, "stderr: %s", stderr.String())
	require.Nil(t, responses["1"].Error, "stderr: %s", stderr.String())
	var created acp.NewSessionResponse
	require.NoError(t, json.Unmarshal(responses["1"].Result, &created))
	// The path is relative so that the model's reply holds no directory: a
	// reply is streamed in pieces, and a directory split across pieces could
	// not be replaced on replay.
	prompt := "Call the write tool exactly once with file_path exactly factory-result.txt, a relative path, and content exactly factory acceptance 7421 with no trailing newline. The file does not exist. Do not call another tool. After the successful tool result, reply with a short plain-text acknowledgment. Never emit tool syntax in your final answer."
	send(2, "session/prompt", map[string]any{"sessionId": created.SessionId, "prompt": []acp.ContentBlock{{Text: &acp.ContentBlockText{Type: "text", Text: prompt}}}})
	responses, notifications := wire.Await("2")
	if recording {
		for _, path := range paths {
			exchange, err := providertest.Load(path)
			require.NoError(t, err)
			require.False(t, exchange.Response.Incomplete)
		}
	}
	require.Nil(t, responses["2"].Error, "response: %+v stderr: %s", responses["2"].Error, stderr.String())
	var result acp.PromptResponse
	require.NoError(t, json.Unmarshal(responses["2"].Result, &result))
	require.Equal(t, acp.StopReasonEndTurn, result.StopReason)
	require.NotNil(t, result.Usage)
	require.Positive(t, result.Usage.TotalTokens)
	body, err := os.ReadFile(filepath.Join(cwd, "factory-result.txt"))
	require.NoError(t, err)
	require.Equal(t, "factory acceptance 7421", string(body))
	require.EqualValues(t, 2, requests.Load())
	var callSeen, resultSeen, textSeen bool
	for _, notification := range notifications {
		if notification.Method != "session/update" {
			continue
		}
		var params acp.SessionNotification
		require.NoError(t, json.Unmarshal(notification.Params, &params))
		if params.Update.ToolCall != nil {
			callSeen = true
			require.Equal(t, acp.ToolKindEdit, params.Update.ToolCall.Kind)
		}
		if params.Update.ToolCallUpdate != nil && params.Update.ToolCallUpdate.Status != nil && *params.Update.ToolCallUpdate.Status == acp.ToolCallStatusCompleted {
			resultSeen = true
		}
		if params.Update.AgentMessageChunk != nil {
			textSeen = true
		}
	}
	require.True(t, callSeen)
	require.True(t, resultSeen)
	require.True(t, textSeen)
	wire.Close()
	require.NoError(t, command.Wait(), "stderr: %s", stderr.String())
	st, err := dir.New(dir.Options{Root: filepath.Join(home, "store"), Compression: dir.CompressionNone})
	require.NoError(t, err)
	sessionID, err := uuid.Parse(string(created.SessionId))
	require.NoError(t, err)
	seq, err := st.History(t.Context(), sessionID, store.Range{})
	require.NoError(t, err)
	var calls, results, usage, texts, users int
	var total transcript.Usage
	for entry, err := range seq {
		require.NoError(t, err)
		item := entry.Item
		switch {
		case item.User != nil:
			users++
		case item.ToolCall != nil:
			calls++
			require.Equal(t, "write", item.ToolCall.Name)
		case item.ToolResult != nil:
			results++
			require.Equal(t, transcript.OutcomeCompleted, item.ToolResult.Outcome)
		case item.Usage != nil:
			usage++
			total.InputTokens += item.Usage.InputTokens
			total.OutputTokens += item.Usage.OutputTokens
		case item.Text != nil:
			if strings.TrimSpace(item.Text.Text) != "" {
				texts++
			}
		}
	}
	require.Equal(t, 1, users)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, results)
	require.Equal(t, 2, usage)
	require.Positive(t, texts)
	require.EqualValues(t, total.InputTokens, result.Usage.InputTokens)
	require.EqualValues(t, total.OutputTokens, result.Usage.OutputTokens)
}
