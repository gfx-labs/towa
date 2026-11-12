package cli

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/agentctx"
	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/compact"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/embed"
	"github.com/gfx-labs/acpharness/src/hooks"
	"github.com/gfx-labs/acpharness/src/httpclient"
	"github.com/gfx-labs/acpharness/src/jev"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/mcp"
	"github.com/gfx-labs/acpharness/src/models"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/provider/hostedsearch"
	"github.com/gfx-labs/acpharness/src/provider/openai"
	"github.com/gfx-labs/acpharness/src/provider/router"
	"github.com/gfx-labs/acpharness/src/session"
	"github.com/gfx-labs/acpharness/src/store/dir"
	"github.com/gfx-labs/acpharness/src/subagent"
	"github.com/gfx-labs/acpharness/src/telemetry"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/core"
	"github.com/gfx-labs/acpharness/src/tools/extras"
	"github.com/gfx-labs/acpharness/src/toolset"
)

type runtimeFactory struct {
	cfg        config.Config
	host       Host
	tel        *telemetry.Telemetry
	client     *http.Client
	providers  *Providers
	store      *dir.Store
	pool       *mcp.Pool
	mcpOptions mcpRuntimeOptions
	spawner    *subagent.Spawner
	embeddings *embed.Client
	jev        *jev.Client
}

type sessionRuntime struct {
	factory        *runtimeFactory
	manager        *mcp.Manager
	frozen         []tools.Tool
	reads          *core.ReadTracker
	mapper         loop.Mapper
	compactor      *compact.Controller
	compactorModel string
}

type preparedTurn struct {
	resolved      loop.Resolved
	mapper        loop.Mapper
	system        []provider.SystemBlock
	native        map[string]compact.Native
	modelName     string
	summaryWindow int64
	summaryOutput int64
}

func newRuntimeFactory(ctx context.Context, cfg config.Config, h Host, tel *telemetry.Telemetry) (_ *runtimeFactory, err error) {
	f := &runtimeFactory{cfg: cfg, host: h, tel: tel, client: httpclient.New()}
	defer func() {
		if err != nil {
			err = errors.Join(err, f.Close())
		}
	}()
	f.providers, err = NewProviders(cfg, h, f.client)
	if err != nil {
		return nil, err
	}
	root, err := cfg.Store.Dir(h.Environ)
	if err != nil {
		return nil, err
	}
	f.store, err = dir.New(dir.Options{Root: root, Compression: dir.Compression(cfg.Store.Compression)})
	if err != nil {
		return nil, err
	}
	removed, err := f.store.Sweep(ctx)
	if err != nil {
		return nil, fmt.Errorf("session store recovery: %w", err)
	}
	if removed != 0 {
		tel.Logger.Info("removed incomplete session directories", "count", removed)
	}
	tokens, err := authStore(h)
	if err != nil {
		return nil, err
	}
	f.mcpOptions = mcpRuntimeOptions{
		Config: cfg.MCP, HTTPClient: f.client, Tokens: tokens,
		Logger: tel.Logger, Tracer: tel.Tracer, Version: version(),
		LookupEnv: func(name string) (string, bool) {
			for _, entry := range slices.Backward(h.Environ) {
				key, value, found := strings.Cut(entry, "=")
				if found && key == name {
					return value, true
				}
			}
			return "", false
		},
	}
	f.pool, err = newMCPPool(f.mcpOptions)
	if err != nil {
		return nil, err
	}
	f.spawner, err = subagent.New(subagent.Options{Config: cfg.Subagents})
	if err != nil {
		return nil, err
	}
	if cfg.Embeddings.BaseURL != "" {
		f.embeddings, err = embed.New(embed.Options{Config: cfg.Embeddings, HTTPClient: f.client, Credentials: f.providers.Credentials})
		if err != nil {
			return nil, err
		}
	}
	if cfg.Jev.Route != "" {
		f.jev, err = jev.New(jev.Options{Config: cfg.Jev, HTTPClient: f.client})
		if err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (f *runtimeFactory) Close() error {
	var errs []error
	if f.pool != nil {
		errs = append(errs, f.pool.Close())
	}
	if f.providers != nil {
		errs = append(errs, f.providers.Close())
	}
	f.client.CloseIdleConnections()
	return errors.Join(errs...)
}

func (f *runtimeFactory) Open(ctx context.Context, base loop.Turn) (session.Runtime, error) {
	r, err := f.open(ctx, base)
	if err != nil {
		return session.Runtime{}, err
	}
	return session.Runtime{Turn: r.Run, Close: r.Close, Mapper: &r.mapper}, nil
}

func (f *runtimeFactory) open(ctx context.Context, base loop.Turn) (*sessionRuntime, error) {
	r := &sessionRuntime{factory: f, reads: &core.ReadTracker{}}
	var taken []string
	if len(f.cfg.Models) != 0 {
		prepared, err := f.prepare(ctx, base, nil, r.reads)
		if err != nil {
			return nil, err
		}
		for _, tool := range prepared.resolved.Tools.Tools {
			taken = append(taken, tool.Spec.Name)
		}
	}
	manager, frozen, err := newSessionMCP(ctx, f.mcpOptions, f.pool, base, taken)
	if err != nil {
		return nil, err
	}
	r.manager, r.frozen = manager, frozen
	if len(f.cfg.Models) != 0 {
		prepared, err := f.prepare(ctx, base, frozen, r.reads)
		if err != nil {
			return nil, errors.Join(err, r.Close(context.WithoutCancel(ctx)))
		}
		r.mapper = prepared.mapper
	}
	return r, nil
}

func (f *runtimeFactory) Run(ctx context.Context, child loop.Turn) (result loop.Result, err error) {
	runtime, err := f.Open(ctx, child)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, runtime.Close(context.WithoutCancel(ctx))) }()
	return runtime.Turn(ctx, child)
}

func (r *sessionRuntime) Close(context.Context) error {
	if r.compactor != nil {
		r.compactor.Close()
	}
	return r.manager.Close()
}

func (r *sessionRuntime) Run(ctx context.Context, turn loop.Turn) (loop.Result, error) {
	var prepared preparedTurn
	f := r.factory
	l, err := loop.New(loop.Options{
		Resolve: func(ctx context.Context, model string) (loop.Resolved, error) {
			selected := turn
			selected.Model = model
			var err error
			prepared, err = f.prepare(ctx, selected, r.frozen, r.reads)
			return prepared.resolved, err
		},
		Store: f.store, System: func(context.Context, loop.Turn) ([]provider.SystemBlock, error) { return prepared.system, nil },
		Limits: f.cfg.Limits, Now: time.Now, Repair: f.cfg.Tools.Repair.Enabled,
		Gate: f.cfg.Tools.Gate, Hooks: hooks.New(f.cfg.Hooks), Logger: f.tel.Logger,
		NewCompactor: func(context.Context, loop.Turn, loop.Resolved) (*compact.Controller, error) {
			if r.compactor != nil && r.compactorModel == prepared.modelName {
				return r.compactor, nil
			}
			if r.compactor != nil {
				r.compactor.Close()
				r.compactor = nil
			}
			var embeddings compact.Embedder
			if f.embeddings != nil {
				embeddings = f.embeddings
			}
			controller, err := compact.NewController(compact.ControllerOptions{
				Config: f.cfg.Compaction, Summary: prepared.resolved.Provider,
				SummaryContextWindow: prepared.summaryWindow,
				SummaryInput: compact.SummaryInput{
					Model: prepared.resolved.Model, MaxOutputTokens: prepared.summaryOutput,
					Effort: prepared.resolved.Effort, ConversationSearch: f.cfg.ConversationSearch.Enabled,
				},
				Embedder: embeddings, Native: func(name string) compact.Native { return prepared.native[name] },
			})
			if err != nil {
				return nil, err
			}
			r.compactor, r.compactorModel = controller, prepared.modelName
			return controller, nil
		},
	})
	if err != nil {
		return loop.Result{}, err
	}
	return l.Run(ctx, turn)
}

func (f *runtimeFactory) prepare(ctx context.Context, turn loop.Turn, frozen []tools.Tool, reads *core.ReadTracker) (preparedTurn, error) {
	if reads == nil {
		return preparedTurn{}, errors.New("session read tracker is required")
	}
	selected, err := models.Select(f.cfg.DefaultModel, f.cfg.Models, turn.Model)
	if err != nil {
		return preparedTurn{}, err
	}
	chain, err := selected.Chain(f.providers.Client)
	if err != nil {
		return preparedTurn{}, err
	}
	contextConfig := f.cfg.Context
	contextConfig.ExtraFiles = append(slices.Clone(contextConfig.ExtraFiles), turn.ContextFiles...)
	contextConfig.SkillsDirs = append(slices.Clone(contextConfig.SkillsDirs), turn.SkillsDirs...)
	loaded, err := agentctx.Load(agentctx.Request{Config: contextConfig, WorkDir: turn.Cwd, HomeDir: lookup(f.host.Environ, "HOME")})
	if err != nil {
		return preparedTurn{}, err
	}
	env := tools.Env{SessionID: turn.SessionID, Cwd: turn.Cwd, Dirs: slices.Clone(turn.Dirs)}
	showPlan := func(ctx context.Context, env tools.Env, entries []acp.PlanEntry) error {
		current, ok := loop.Current(ctx)
		if !ok || current.Turn.SessionID != env.SessionID {
			return errors.New("plan update has no matching current turn")
		}
		return current.Turn.Update(ctx, acp.SessionUpdate{Plan: &acp.SessionUpdatePlan{Entries: entries}})
	}
	coreTools, err := core.Tools(env, f.cfg.Tools, core.Deps{Reads: reads, ShowPlan: showPlan})
	if err != nil {
		return preparedTurn{}, err
	}
	additional, err := f.extraTools(loaded)
	if err != nil {
		return preparedTurn{}, err
	}
	assembled, err := toolset.Assemble(ctx, toolset.Input{
		Tools:  func() ([]tools.Tool, error) { return coreTools, nil },
		Extras: additional, MCP: frozen, Gate: f.cfg.Tools.Gate,
	})
	if err != nil {
		return preparedTurn{}, err
	}
	result := preparedTurn{
		native: make(map[string]compact.Native), modelName: selected.Name,
		summaryWindow: selected.Entries[0].Metadata.ContextWindow,
		summaryOutput: selected.Entries[0].Metadata.MaxOutputTokens,
	}
	for i, entry := range selected.Entries {
		result.summaryWindow = min(result.summaryWindow, entry.Metadata.ContextWindow)
		result.summaryOutput = min(result.summaryOutput, entry.Metadata.MaxOutputTokens)
		skillTool := ""
		if f.cfg.Context.SkillsEnabled {
			skillTool = extras.NameSkill
		}
		hosted, hasHosted, err := hostedsearch.Tool(f.cfg.WebSearch, entry.Provider, entry.Model)
		if err != nil {
			return preparedTurn{}, err
		}
		system, err := agentctx.Build(agentctx.Prompt{
			Instructions: agentctx.Instructions, Tools: agentctx.ToolGuidance,
			ToolLines: toolLines(assembled.Set, hasHosted), Loaded: loaded,
			WorkDir: turn.Cwd, AdditionalDirs: turn.Dirs, SkillTool: skillTool, Client: turn.SystemPrompt,
		})
		if err != nil {
			return preparedTurn{}, err
		}
		chain[i].Preparation = &router.Preparation{
			ContextWindow: entry.Metadata.ContextWindow, MaxOutputTokens: entry.Metadata.MaxOutputTokens,
			Interruption: agentctx.Interruption, System: system,
		}
		if hasHosted {
			chain[i].Preparation.HostedSearch, chain[i].Preparation.LocalSearch = hosted, extras.NameWebSearch
		}
		if i == 0 {
			result.system = system
		}
		if client, ok := chain[i].Provider.(*openai.Client); ok {
			scheme, err := f.accountScheme(entry.Account)
			if err != nil {
				return preparedTurn{}, err
			}
			name, err := client.ContextName(auth.Credential{Account: entry.Account, Scheme: scheme})
			if err != nil {
				return preparedTurn{}, err
			}
			result.native[name] = compact.NativeFunc(func(ctx context.Context, request provider.Request) (compact.NativeResult, error) {
				compacted, err := client.Compact(ctx, request)
				if err != nil {
					return compact.NativeResult{}, err
				}
				if compacted == nil {
					return compact.NativeResult{}, errors.New("native compaction returned no result")
				}
				return compact.NativeResult{Items: compacted.Items, Usage: compacted.Usage}, nil
			})
		}
	}
	routed, err := router.New(router.Options{Chain: chain, Auth: f.providers.Credentials, Retry: f.cfg.Limits.Retry, Rand: rand.Float64})
	if err != nil {
		return preparedTurn{}, err
	}
	first := selected.Entries[0]
	result.resolved = loop.Resolved{
		Provider: routed, Tools: assembled.Set, Model: first.Model,
		ProviderName: first.Provider, ContextSize: first.Metadata.ContextWindow, MaxOutputTokens: first.Metadata.MaxOutputTokens,
		Interruption: agentctx.Interruption,
	}
	result.mapper = loop.Mapper{
		Tools: assembled.Set,
		ContextSize: func(model string) (int, bool) {
			for _, entry := range selected.Entries {
				if entry.Model == model {
					return int(entry.Metadata.ContextWindow), true
				}
			}
			return 0, false
		},
	}
	return result, nil
}

func (f *runtimeFactory) accountScheme(id string) (auth.Scheme, error) {
	for _, account := range f.cfg.Accounts {
		if account.ID != id {
			continue
		}
		switch account.Kind {
		case config.AccountOAuth, config.AccountProxiedOAuth:
			return auth.SchemeOAuth, nil
		case config.AccountAPIKey, config.AccountProxiedAPIKey:
			return auth.SchemeAPIKey, nil
		case config.AccountNone:
			return auth.SchemeNone, nil
		default:
			return "", fmt.Errorf("account %q has unsupported kind %q", id, account.Kind)
		}
	}
	return "", fmt.Errorf("account %q is not configured", id)
}

// toolLines returns the prompt's guidance for the tools one chain entry is
// sent. An entry with hosted search gets the provider's tool in place of
// the local one, so its line names that tool.
func toolLines(set tools.Set, hostedSearch bool) []string {
	var lines []string
	for _, tool := range set.Tools {
		if hostedSearch && tool.Spec.Name == extras.NameWebSearch {
			lines = append(lines, extras.HostedSearchGuidance(hostedsearch.Name))
			continue
		}
		lines = append(lines, tool.Guidance...)
	}
	return lines
}

func (f *runtimeFactory) extraTools(loaded *agentctx.Loaded) ([]toolset.Extra, error) {
	var out []toolset.Extra
	add := func(name string, tool tools.Tool, err error) error {
		if err != nil {
			return err
		}
		out = append(out, toolset.Extra{Name: name, Tools: []tools.Tool{tool}})
		return nil
	}
	if f.cfg.ConversationSearch.Enabled {
		tool, err := extras.ConversationSearch(f.cfg.ConversationSearch, f.store)
		if err := add(extras.NameConversationSearch, tool, err); err != nil {
			return nil, err
		}
	}
	if f.cfg.WebFetch.Enabled {
		tool, err := extras.WebFetch(f.cfg.WebFetch, f.client)
		if err := add(extras.NameWebFetch, tool, err); err != nil {
			return nil, err
		}
	}
	if f.cfg.WebSearch.Enabled {
		tool, err := extras.WebSearch(f.cfg.WebSearch, f.client)
		if err := add(extras.NameWebSearch, tool, err); err != nil {
			return nil, err
		}
	}
	if f.cfg.Context.SkillsEnabled {
		skills := loaded.Skills
		if skills == nil {
			skills = []agentctx.Skill{}
		}
		tool, err := extras.Skill(f.cfg.Context, skills)
		if err := add(extras.NameSkill, tool, err); err != nil {
			return nil, err
		}
	}
	if f.jev != nil {
		tool, err := extras.JevDecision(f.jev, func(ctx context.Context) (auth.Credential, error) {
			if f.cfg.Jev.Account == "" {
				return auth.Credential{}, nil
			}
			return f.providers.Credentials.Credential(ctx, f.cfg.Jev.Account)
		})
		if err := add(extras.NameJevDecision, tool, err); err != nil {
			return nil, err
		}
	}
	if f.cfg.Tools.Batch.Enabled {
		tool, err := extras.Batch(f.cfg.Tools.Batch, loop.BatchDispatcher)
		if err := add(extras.NameBatch, tool, err); err != nil {
			return nil, err
		}
	}
	if f.cfg.Subagents.MaxDepth != 0 {
		tool, err := extras.Subagent(f.spawner, f)
		if err := add(extras.NameSubagent, tool, err); err != nil {
			return nil, err
		}
	}
	return out, nil
}
