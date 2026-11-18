package cli

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/towa/src/agent/extras"
	"github.com/gfx-labs/towa/src/agent/hooks"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/agent/session"
	"github.com/gfx-labs/towa/src/agent/subagent"
	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/prompt/agentctx"
	"github.com/gfx-labs/towa/src/prompt/compact"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/hostedimage"
	"github.com/gfx-labs/towa/src/provider/hostedsearch"
	"github.com/gfx-labs/towa/src/provider/models"
	"github.com/gfx-labs/towa/src/provider/openai"
	"github.com/gfx-labs/towa/src/provider/router"
	"github.com/gfx-labs/towa/src/sidecar/embed"
	"github.com/gfx-labs/towa/src/sidecar/jev"
	"github.com/gfx-labs/towa/src/store/dir"
	"github.com/gfx-labs/towa/src/support/httpclient"
	"github.com/gfx-labs/towa/src/support/telemetry"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/core"
	"github.com/gfx-labs/towa/src/tools/jobs"
	"github.com/gfx-labs/towa/src/tools/mcp"
	"github.com/gfx-labs/towa/src/tools/toolset"
)

// runtimeFactory opens session runtimes. What it holds lives as long as the
// process. What a configuration builds is in its current generation, which
// reload replaces.
type runtimeFactory struct {
	host       Host
	tel        *telemetry.Telemetry
	client     *http.Client
	store      *dir.Store
	pool       *mcp.Pool
	mcpOptions mcpRuntimeOptions
	// skills and schemas hold what every session reads or builds the same
	// way, so that a session keeps no copy of its own.
	skills  agentctx.SkillCache
	schemas toolset.Cache
	current atomic.Pointer[generation]
}

// generation is what one configuration builds. A turn reads the generation
// that is current when it starts, so a reload applies from the next turn of
// every session.
type generation struct {
	*runtimeFactory
	cfg       config.Config
	providers *Providers
	// unlisted is where the figures of a model the built-in table does not
	// have come from. It is resolved when the generation is built.
	unlisted   models.Unlisted
	spawner    *subagent.Spawner
	embeddings *embed.Client
	jev        *jev.Client
}

type sessionRuntime struct {
	factory *runtimeFactory
	manager *mcp.Manager
	frozen  []tools.Tool
	reads   *core.ReadTracker
	jobs    *jobs.Set
	// loaded is the context files, the skill listing and the configured
	// sections as they were when the session opened. Every turn uses them,
	// so a file edited during the session does not change the prompt, and
	// with it the provider's cached prefix, until a new session.
	loaded *agentctx.Loaded
	// prompts keeps the system prompt built from loaded between turns.
	prompts      promptCache
	mapper       loop.Mapper
	compactor    *compact.Controller
	compactorKey string
	// kept is the session's history between turns.
	kept *loop.Kept
}

type preparedTurn struct {
	resolved      loop.Resolved
	mapper        loop.Mapper
	system        []provider.SystemBlock
	native        map[string]compact.Native
	compactorKey  string
	summaryWindow int64
	summaryOutput int64
}

func newRuntimeFactory(ctx context.Context, cfg config.Config, h Host, tel *telemetry.Telemetry) (_ *runtimeFactory, err error) {
	f := &runtimeFactory{host: h, tel: tel, client: httpclient.New()}
	defer func() {
		if err != nil {
			err = errors.Join(err, f.Close())
		}
	}()
	root, err := cfg.Store.Dir(h.Environ)
	if err != nil {
		return nil, err
	}
	f.store, err = dir.New(dir.Options{
		Root: root, Compression: dir.Compression(cfg.Store.Compression), NoSync: !cfg.Store.Fsync,
		ZstdConcurrency: cfg.Store.ZstdConcurrency,
	})
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
	g, err := f.newGeneration(ctx, cfg)
	if err != nil {
		return nil, err
	}
	f.current.Store(g)
	return f, nil
}

// newGeneration builds what cfg configures for a turn: the providers, the
// model figures and the sidecar clients.
func (f *runtimeFactory) newGeneration(ctx context.Context, cfg config.Config) (_ *generation, err error) {
	g := &generation{runtimeFactory: f, cfg: cfg}
	defer func() {
		if err != nil {
			err = errors.Join(err, g.close())
		}
	}()
	g.providers, err = NewProviders(ctx, cfg, f.host, f.client)
	if err != nil {
		return nil, err
	}
	g.unlisted, err = g.unlistedModels(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	g.providers.lists.Store(&g.unlisted.Lists)
	g.spawner, err = subagent.New(subagent.Options{Config: cfg.Subagents})
	if err != nil {
		return nil, err
	}
	if cfg.Embeddings.BaseURL != "" {
		g.embeddings, err = embed.New(embed.Options{Config: cfg.Embeddings, HTTPClient: f.client, Credentials: g.providers.Credentials})
		if err != nil {
			return nil, err
		}
	}
	if cfg.Jev.Route != "" {
		g.jev, err = jev.New(jev.Options{Config: cfg.Jev, HTTPClient: f.client})
		if err != nil {
			return nil, err
		}
	}
	return g, nil
}

// reload makes a generation built from cfg the current one. A turn that
// started on the one it replaces finishes on it: closing a provider only
// stops it keeping idle connections.
func (f *runtimeFactory) reload(ctx context.Context, cfg config.Config) (*generation, error) {
	g, err := f.newGeneration(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return g, f.current.Swap(g).close()
}

func (f *generation) close() error {
	if f.providers == nil {
		return nil
	}
	return f.providers.Close()
}

func (f *runtimeFactory) Close() error {
	var errs []error
	if f.pool != nil {
		errs = append(errs, f.pool.Close())
	}
	if g := f.current.Load(); g != nil {
		errs = append(errs, g.close())
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
	g := f.current.Load()
	r := &sessionRuntime{
		factory: f, reads: &core.ReadTracker{}, jobs: jobs.NewSet(),
		kept: loop.NewKept(time.Duration(g.cfg.Session.KeepHistorySeconds) * time.Second),
	}
	var taken []string
	if len(g.cfg.Models) != 0 {
		loaded, err := g.loadContext(base)
		if err != nil {
			return nil, err
		}
		r.loaded = loaded
		prepared, err := g.prepare(ctx, turnSetup{turn: base, reads: r.reads, jobs: r.jobs, loaded: r.loaded, prompts: &r.prompts})
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
	if len(g.cfg.Models) != 0 {
		prepared, err := g.prepare(ctx, turnSetup{turn: base, frozen: frozen, reads: r.reads, jobs: r.jobs, loaded: r.loaded, prompts: &r.prompts})
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
	r.kept.Drop()
	if r.compactor != nil {
		r.compactor.Close()
	}
	return errors.Join(r.jobs.Close(), r.manager.Close())
}

func (r *sessionRuntime) Run(ctx context.Context, turn loop.Turn) (loop.Result, error) {
	var prepared preparedTurn
	f := r.factory.current.Load()
	l, err := loop.New(loop.Options{
		Resolve: func(ctx context.Context, model string) (loop.Resolved, error) {
			selected := turn
			selected.Model = model
			var err error
			prepared, err = f.prepare(ctx, turnSetup{turn: selected, frozen: r.frozen, reads: r.reads, jobs: r.jobs, loaded: r.loaded, prompts: &r.prompts})
			return prepared.resolved, err
		},
		Store: f.store, System: func(context.Context, loop.Turn) ([]provider.SystemBlock, error) { return prepared.system, nil },
		Limits: f.cfg.Limits, Now: time.Now, Repair: f.cfg.Tools.Repair.Enabled,
		Gate: f.cfg.Tools.Gate, Hooks: hooks.New(f.cfg.Hooks), Logger: f.tel.Logger, Jobs: r.jobs, Kept: r.kept,
		NewCompactor: func(context.Context, loop.Turn, loop.Resolved) (*compact.Controller, error) {
			if r.compactor != nil && r.compactorKey == prepared.compactorKey {
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
			r.compactor, r.compactorKey = controller, prepared.compactorKey
			return controller, nil
		},
	})
	if err != nil {
		return loop.Result{}, err
	}
	return l.Run(ctx, turn)
}

// loadContext reads what a session's prompt takes from the disk: the context
// files, the skill listing and the configured sections.
func (f *generation) loadContext(turn loop.Turn) (*agentctx.Loaded, error) {
	contextConfig := f.cfg.Context
	contextConfig.Agents = append(slices.Clone(contextConfig.Agents), turn.ContextFiles...)
	contextConfig.SkillsDirs = append(slices.Clone(contextConfig.SkillsDirs), turn.SkillsDirs...)
	return agentctx.Load(agentctx.Request{
		Config: contextConfig, WorkDir: turn.Cwd, HomeDir: lookup(f.host.Environ, "HOME"), Skills: &f.skills,
	})
}

// promptCache keeps the system prompts one session has built. A session's
// prompt differs between model entries only in whether web search is the
// provider's own and whether the provider generates images, so there is one
// place for each combination. A prompt is built again
// when anything it is made from differs from what the kept one was made
// from, which is the case once: when the session's MCP tools arrive.
type promptCache struct {
	kept map[hostedTools]*builtPrompt
}

// hostedTools says which of the provider's own tools an entry is sent.
type hostedTools struct{ search, image bool }

type builtPrompt struct {
	from   agentctx.Prompt
	system []provider.SystemBlock
}

func (c *promptCache) build(hosted hostedTools, p agentctx.Prompt) ([]provider.SystemBlock, error) {
	if b := c.kept[hosted]; b != nil && b.from.Instructions == p.Instructions && slices.Equal(b.from.Tools, p.Tools) &&
		slices.Equal(b.from.Rules, p.Rules) && b.from.Loaded == p.Loaded && b.from.WorkDir == p.WorkDir &&
		slices.Equal(b.from.AdditionalDirs, p.AdditionalDirs) && b.from.SkillTool == p.SkillTool &&
		reflect.DeepEqual(b.from.Client, p.Client) {
		return b.system, nil
	}
	system, err := agentctx.Build(p)
	if err != nil {
		return nil, err
	}
	p.AdditionalDirs = slices.Clone(p.AdditionalDirs)
	if c.kept == nil {
		c.kept = map[hostedTools]*builtPrompt{}
	}
	c.kept[hosted] = &builtPrompt{from: p, system: system}
	return system, nil
}

// turnSetup is what prepare reads besides the factory: the turn, the MCP
// tools frozen for the session, and the session's own state.
type turnSetup struct {
	turn    loop.Turn
	frozen  []tools.Tool
	reads   *core.ReadTracker
	jobs    *jobs.Set
	loaded  *agentctx.Loaded
	prompts *promptCache
}

func (f *generation) prepare(ctx context.Context, s turnSetup) (preparedTurn, error) {
	if s.reads == nil {
		return preparedTurn{}, errors.New("session read tracker is required")
	}
	selected, err := models.Select(f.cfg.Models, f.unlisted, s.turn.Model)
	if err != nil {
		return preparedTurn{}, err
	}
	if s.turn.Effort != "" {
		selected.Entries[0].Effort = s.turn.Effort
	}
	chain, err := selected.Chain(f.providers.Client)
	if err != nil {
		return preparedTurn{}, err
	}
	if s.loaded == nil || s.prompts == nil {
		return preparedTurn{}, errors.New("session context is required")
	}
	assembled, err := f.assembleTools(ctx, s)
	if err != nil {
		return preparedTurn{}, err
	}
	first := selected.Entries[0]
	result := preparedTurn{
		native: make(map[string]compact.Native), compactorKey: selected.Name + "#" + first.Effort,
		summaryWindow: first.Metadata.ContextWindow,
		summaryOutput: first.Metadata.MaxOutputTokens,
	}
	for i, entry := range selected.Entries {
		result.summaryWindow = min(result.summaryWindow, entry.Metadata.ContextWindow)
		result.summaryOutput = min(result.summaryOutput, entry.Metadata.MaxOutputTokens)
		system, err := f.prepareEntry(&chain[i], entry, assembled.Set, s, &result)
		if err != nil {
			return preparedTurn{}, err
		}
		if i == 0 {
			result.system = system
		}
	}
	routed, err := router.New(router.Options{Chain: chain, Auth: f.providers.Credentials, Retry: f.cfg.Limits.Retry, Rand: rand.Float64})
	if err != nil {
		return preparedTurn{}, err
	}
	result.resolved = loop.Resolved{
		Provider: routed, Tools: assembled.Set, Schemas: assembled.Schemas, Model: first.Model,
		ProviderName: first.Provider, ContextSize: first.Metadata.ContextWindow, MaxOutputTokens: first.Metadata.MaxOutputTokens,
		Effort: first.Effort, Interruption: agentctx.Interruption,
	}
	result.mapper = loop.Mapper{Tools: assembled.Set, ContextSize: contextSizeOf(selected.Entries)}
	return result, nil
}

// assembleTools builds the turn's tool set from the core tools, the
// extras and the session's MCP tools.
func (f *generation) assembleTools(ctx context.Context, s turnSetup) (toolset.Result, error) {
	env := tools.Env{SessionID: s.turn.SessionID, Cwd: s.turn.Cwd, Dirs: slices.Clone(s.turn.Dirs)}
	coreTools, err := core.Tools(env, f.cfg.Tools, core.Deps{Reads: s.reads, Jobs: s.jobs, ShowPlan: showPlan})
	if err != nil {
		return toolset.Result{}, err
	}
	additional, err := f.extraTools(s.loaded)
	if err != nil {
		return toolset.Result{}, err
	}
	return toolset.Assemble(ctx, toolset.Input{
		Tools:  func() ([]tools.Tool, error) { return coreTools, nil },
		Extras: additional, MCP: s.frozen, Gate: f.cfg.Tools.Gate, Schemas: &f.schemas,
	})
}

// showPlan sends a plan update on the turn that is running.
func showPlan(ctx context.Context, env tools.Env, entries []acp.PlanEntry) error {
	current, ok := loop.Current(ctx)
	if !ok || current.Turn.SessionID != env.SessionID {
		return errors.New("plan update has no matching current turn")
	}
	return current.Turn.Update(ctx, acp.SessionUpdate{Plan: &acp.SessionUpdatePlan{Entries: entries}})
}

// prepareEntry fills in one chain entry's preparation and returns its
// system prompt. An entry on an OpenAI client also adds its native
// compaction to result.
func (f *generation) prepareEntry(chained *router.Entry, entry models.Entry, set tools.Set, s turnSetup, result *preparedTurn) ([]provider.SystemBlock, error) {
	skillTool := ""
	if f.cfg.Context.SkillsEnabled {
		skillTool = extras.NameSkill
	}
	hosted, hasHosted, err := hostedsearch.Tool(f.cfg.WebSearch, entry.Provider, entry.Model)
	if err != nil {
		return nil, err
	}
	image, hasImage, err := hostedimage.Tool(f.cfg.ImageGeneration, entry.Provider, entry.Model, entry.InputModalities)
	if err != nil {
		return nil, err
	}
	listed, rules := toolSections(set, hasHosted, hasImage, f.cfg.ImageGeneration.Enabled)
	system, err := s.prompts.build(hostedTools{search: hasHosted, image: hasImage}, agentctx.Prompt{
		Instructions: s.loaded.Instructions, Tools: listed, Rules: rules, Loaded: s.loaded,
		WorkDir: s.turn.Cwd, AdditionalDirs: s.turn.Dirs, SkillTool: skillTool, Client: s.turn.SystemPrompt,
	})
	if err != nil {
		return nil, err
	}
	chained.Preparation = &router.Preparation{
		ContextWindow: entry.Metadata.ContextWindow, MaxOutputTokens: entry.Metadata.MaxOutputTokens,
		Interruption: agentctx.Interruption, System: system,
	}
	if hasHosted {
		chained.Preparation.HostedSearch, chained.Preparation.LocalSearch = hosted, extras.NameWebSearch
	}
	if hasImage {
		chained.Preparation.HostedImage = image
	}
	if client, ok := chained.Provider.(*openai.Client); ok {
		if err := f.addNative(result, client, entry); err != nil {
			return nil, err
		}
	}
	return system, nil
}

// addNative registers the client's compaction under the name its context
// is known by.
func (f *generation) addNative(result *preparedTurn, client *openai.Client, entry models.Entry) error {
	scheme, err := f.accountScheme(entry.Account)
	if err != nil {
		return err
	}
	name, err := client.ContextName(auth.Credential{Account: entry.Account, Scheme: scheme})
	if err != nil {
		return err
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
	return nil
}

// contextSizeOf returns the context window of a model among the entries.
func contextSizeOf(entries []models.Entry) func(model string) (int, bool) {
	return func(model string) (int, bool) {
		for _, entry := range entries {
			if entry.Model == model {
				return int(entry.Metadata.ContextWindow), true
			}
		}
		return 0, false
	}
}

func (f *generation) accountScheme(id string) (auth.Scheme, error) {
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

// toolSections returns the prompt's list of the tools one chain entry is
// sent and its rules: each tool's guidance, then the rules that hold for
// every tool. An entry with hosted search gets the provider's tool in place
// of the local one, so its lines name that tool. A tool without a summary,
// such as an MCP server's, is not listed: its definition describes it.
// Image generation is the provider's own where it is offered at all. When it
// is enabled and this entry has none, a rule says so.
func toolSections(set tools.Set, hostedSearch, hostedImage, imageEnabled bool) (listed []agentctx.ToolLine, rules []string) {
	for _, tool := range set.Tools {
		if hostedSearch && tool.Spec.Name == extras.NameWebSearch {
			listed = append(listed, agentctx.ToolLine{Name: hostedsearch.Name, Summary: extras.SearchSummary})
			rules = append(rules, extras.HostedSearchGuidance(hostedsearch.Name))
			continue
		}
		if tool.Summary != "" {
			listed = append(listed, agentctx.ToolLine{Name: tool.Spec.Name, Summary: tool.Summary})
		}
		rules = append(rules, tool.Guidance...)
	}
	switch {
	case hostedImage:
		listed = append(listed, agentctx.ToolLine{Name: hostedimage.Name, Summary: hostedimage.Summary})
		rules = append(rules, hostedimage.Guidance)
	case imageEnabled:
		rules = append(rules, hostedimage.Unavailable)
	}
	return listed, append(rules, agentctx.GeneralRules...)
}

func (f *generation) extraTools(loaded *agentctx.Loaded) ([]toolset.Extra, error) {
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
