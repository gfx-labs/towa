// Package looptest builds a real loop for tests: the Chat Completions
// client, the router, the directory store and one real tool, answering from
// exchanges recorded from a real server. Nothing in it stands in for a
// provider.
//
// The recordings were made from the build machine's Ollama server with
// llama3.2:3b. They show that a whole turn runs against a real server's
// stream. They show nothing about another provider's errors, headers,
// reasoning items or limits.
package looptest

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/hooks"
	"github.com/gfx-labs/acpharness/src/httpclient"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/provider/chat"
	"github.com/gfx-labs/acpharness/src/provider/providertest"
	"github.com/gfx-labs/acpharness/src/provider/router"
	"github.com/gfx-labs/acpharness/src/store/dir"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/fsops"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// EnvRecord names the variable that must be set for the recordings to be
// made again from the Ollama server.
const EnvRecord = "ACPHARNESS_RECORD_OLLAMA"

// Facts of the recorded server.
const (
	// OllamaBase is the server's Chat Completions root.
	OllamaBase = "http://127.0.0.1:11434/v1"
	// Model is the model the recordings were made with.
	Model = "llama3.2:3b"
	// ContextSize is the model's llama.context_length, as the server's
	// /api/show reported it on 2026-11-11.
	ContextSize = 131072
	// Account is the id of the one account. The server takes any key.
	Account = "local"
	// ProviderID tags the items the client produces.
	ProviderID = "ollama"
	// Interruption is what the fixture's resolver gives as the
	// interruption text. No dialect is on master to take one from.
	Interruption = "The previous turn was interrupted by the user."
)

// Scenario is one recorded turn.
type Scenario struct {
	// Name is the stem of the recordings: testdata/<Name>_<n>.json.
	Name string
	// Requests is how many model requests the turn makes.
	Requests int
	// Model is the provider's model name the turn asks for.
	Model string
	// Prompt is the user's text.
	Prompt string
	// System is the one system block's text, when not empty.
	System string
	// Tools says the turn offers the read_file tool.
	Tools bool
	// MaxOutputTokens is sent with each request when not zero.
	MaxOutputTokens int64
	// Files are written into the working directory before the turn.
	Files map[string]string
}

// The recorded turns.
var (
	// ReadTurn: the model calls read_file on notes.txt, gets its text and
	// answers. Two requests.
	ReadTurn = Scenario{
		Name: "read_turn", Requests: 2, Model: Model, Tools: true, MaxOutputTokens: 256,
		System: "You are a coding agent. Call read_file exactly once to read the requested file. After the tool returns its content, answer the user's question from that content in plain text. Never repeat a tool call. Do not emit tool syntax in a text answer.",
		Prompt: "Use the read_file tool to read the file notes.txt, then tell me the launch code it contains.",
		Files:  map[string]string{"notes.txt": "The launch code is 7421.\n"},
	}
	// UnknownModel: the server answers 404 for a model it does not have.
	UnknownModel = Scenario{Name: "unknown_model", Requests: 1, Model: "acpharness-no-such-model", Prompt: "hi"}
)

// Fixture is a loop wired to a scenario's recordings.
type Fixture struct {
	Loop  *loop.Loop
	Store *dir.Store
	// Cwd is the working directory, holding the scenario's files.
	Cwd string
	// Config is the default configuration the loop was built from.
	Config config.Config
	// Options is what the loop was built from. A test that needs a loop
	// with other limits or hooks changes a copy and calls loop.New.
	Options loop.Options
	// Served counts the model requests sent so far.
	Served func() int

	scenario Scenario
}

// Turn returns the scenario's prompt as a turn of the given session, with
// its updates sent to update.
func (f *Fixture) Turn(session acp.SessionId, update func(context.Context, acp.SessionUpdate) error) loop.Turn {
	return loop.Turn{
		SessionID: session,
		Cwd:       f.Cwd,
		Prompt:    []acp.ContentBlock{{Text: &acp.ContentBlockText{Type: "text", Text: f.scenario.Prompt}}},
		Update:    update,
	}
}

// Replay returns a fixture whose model requests are answered from the
// scenario's recordings, in order. Each request is compared with the
// recorded one, and the test fails when they differ.
func Replay(t testing.TB, s Scenario) *Fixture {
	t.Helper()
	hosts := make([]*url.URL, s.Requests)
	for i := range hosts {
		path := RecordingPath(s, i+1)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			t.Skipf("%s is not recorded: run TestRecord in src/loop/looptest with %s set", path, EnvRecord)
		}
		server := providertest.Replay(t, path)
		u, err := url.Parse(server.URL)
		if err != nil {
			t.Fatalf("looptest: %v", err)
		}
		hosts[i] = u
	}
	return build(t, s, "http://replay.invalid/v1", func(next http.RoundTripper) *sequence {
		return &sequence{next: next, hosts: hosts}
	})
}

// RecordingPath is the file of a scenario's nth request, from 1.
func RecordingPath(s Scenario, n int) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("looptest: no caller information")
	}
	return filepath.Join(filepath.Dir(file), "testdata", fmt.Sprintf("%s_%d.json", s.Name, n))
}

// sequence sends the nth request of a turn to its own place: the nth
// replay server, or the real server through the nth recorder. A request
// past the scenario's count is refused, so a turn cannot run on.
type sequence struct {
	next http.RoundTripper
	// hosts are the replay servers. recorders are used when it is empty.
	hosts     []*url.URL
	recorders []*providertest.Recorder

	mu     sync.Mutex
	served int
}

func (s *sequence) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	n := s.served
	s.served++
	s.mu.Unlock()
	if len(s.hosts) > 0 {
		if n >= len(s.hosts) {
			return nil, fmt.Errorf("looptest: request %d has no recording", n+1)
		}
		// RoundTrip must not modify the caller's request.
		req = req.Clone(req.Context())
		req.URL.Scheme, req.URL.Host, req.Host = s.hosts[n].Scheme, s.hosts[n].Host, ""
		return s.next.RoundTrip(req)
	}
	if n >= len(s.recorders) {
		return nil, fmt.Errorf("looptest: request %d is more than the scenario's %d", n+1, len(s.recorders))
	}
	return s.recorders[n].RoundTrip(req)
}

func (s *sequence) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served
}

func build(t testing.TB, s Scenario, base string, transport func(next http.RoundTripper) *sequence) *Fixture {
	t.Helper()
	fail := func(err error) {
		if err != nil {
			t.Helper()
			t.Fatalf("looptest: %v", err)
		}
	}
	loaded, err := config.Load(config.Sources{})
	fail(err)
	cfg := loaded.Config

	cwd := t.TempDir()
	for name, text := range s.Files {
		fail(os.WriteFile(filepath.Join(cwd, name), []byte(text), 0o600))
	}

	httpClient := httpclient.New()
	seq := transport(httpClient.Transport)
	httpClient.Transport = seq
	client, err := chat.New(chat.Config{
		HTTPClient: httpClient,
		Provider:   config.Provider{ID: ProviderID, Kind: config.ProviderChat, BaseURL: base},
		UserAgent:  "acpharness/test",
	})
	fail(err)

	source, err := auth.NewSource(auth.Options{
		Accounts:   []config.Account{{ID: Account, Provider: ProviderID, Kind: config.AccountAPIKey, APIKeyEnv: "OLLAMA_API_KEY"}},
		Store:      auth.NewStore(t.TempDir()),
		HTTPClient: httpclient.New(),
		// The server takes any key, so this one is not a secret.
		Getenv:       func(string) string { return "ollama" },
		TokenTimeout: time.Minute,
	})
	fail(err)
	routed, err := router.New(router.Options{
		Chain: []router.Entry{{Provider: client, ProviderID: ProviderID, Account: Account, Model: s.Model}},
		Auth:  source,
		Retry: cfg.Limits.Retry,
		Rand:  rand.Float64,
	})
	fail(err)

	st, err := dir.New(dir.Options{Root: t.TempDir(), Compression: dir.CompressionNone})
	fail(err)

	var set tools.Set
	if s.Tools {
		set = tools.Set{Dialect: config.DialectGeneric, Tools: []tools.Tool{ReadTool(cfg.Tools.Files)}}
	}
	opts := loop.Options{
		Resolve: func(_ context.Context, model string) (loop.Resolved, error) {
			if model != "" {
				return loop.Resolved{}, fmt.Errorf("looptest: the fixture has one model and was asked for %q", model)
			}
			return loop.Resolved{
				Provider: routed, Tools: set, Model: s.Model,
				ContextSize: ContextSize, MaxOutputTokens: s.MaxOutputTokens, Interruption: Interruption,
			}, nil
		},
		Store: st,
		System: func(context.Context, loop.Turn) ([]provider.SystemBlock, error) {
			if s.System == "" {
				return nil, nil
			}
			return []provider.SystemBlock{{Name: "base", Text: s.System}}, nil
		},
		Limits: cfg.Limits,
		Now:    time.Now,
		Repair: cfg.Tools.Repair.Enabled,
		Gate:   cfg.Tools.Gate,
		Hooks:  hooks.New(nil),
		Logger: slog.New(slog.DiscardHandler),
	}
	l, err := loop.New(opts)
	fail(err)
	return &Fixture{Loop: l, Store: st, Cwd: cwd, Config: cfg, Options: opts, Served: seq.count, scenario: s}
}

// ReadToolName is the name of the fixture's one tool.
const ReadToolName = "read_file"

// ReadTool is a read of one text file, on the shared file capability. It
// is not a dialect's tool: its name, schema and output are this package's.
func ReadTool(cfg config.ToolsFiles) tools.Tool {
	type input struct {
		Path string `json:"path"`
	}
	parse := func(raw jsontext.Value) (input, error) {
		var in input
		if err := json.Unmarshal(raw, &in); err != nil {
			return input{}, err
		}
		if in.Path == "" {
			return input{}, errors.New("path is empty")
		}
		return in, nil
	}
	return tools.Tool{
		Spec: provider.Tool{
			Name:        ReadToolName,
			Description: "Read a text file. The path is relative to the working directory.",
			InputSchema: jsontext.Value(`{"type":"object","properties":{"path":{"type":"string","description":"The file to read."}},"required":["path"]}`),
		},
		Effect: tools.ReadOnly,
		Describe: func(raw jsontext.Value) (tools.Description, error) {
			in, err := parse(raw)
			if err != nil {
				return tools.Description{}, err
			}
			return tools.Description{
				Title: "Read " + in.Path, Kind: acp.ToolKindRead, Locations: []acp.ToolCallLocation{{Path: in.Path}},
			}, nil
		},
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			in, err := parse(call.Input)
			if err != nil {
				return tools.Result{}, err
			}
			res, err := fsops.Read(ctx, fsops.RootsOf(call.Env, cfg), cfg, fsops.ReadInput{Path: in.Path})
			var refused *fsops.Error
			switch {
			case errors.As(err, &refused):
				return tools.Result{Outcome: transcript.OutcomeFailed, Parts: []transcript.Part{{Text: refused.Error()}}}, nil
			case err != nil:
				return tools.Result{}, err
			case res.Image != nil:
				return tools.Result{Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Image: res.Image}}}, nil
			}
			return tools.Result{Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Text: res.Text}}}, nil
		},
	}
}
