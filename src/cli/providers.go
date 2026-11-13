package cli

import (
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/anthropic"
	"github.com/gfx-labs/towa/src/provider/chat"
	"github.com/gfx-labs/towa/src/provider/models"
	"github.com/gfx-labs/towa/src/provider/openai"
)

// Providers holds the process's clients and credential source. Its Client
// method supplies models.Model.Chain, and Credentials supplies the router.
type Providers struct {
	Credentials auth.Source
	clients     map[string]provider.Provider
	responses   []*openai.Client
	// lists are the providers' model lists. They are read with the clients
	// this holds, so they are set after it is built, and a request reads
	// them for what the table does not say about its model.
	lists atomic.Pointer[models.Lists]
}

// NewProviders builds clients from resolved configuration. client is the
// shared client from httpclient.New. It does not read keys or refresh a
// login until the router asks Credentials for an account.
func NewProviders(cfg config.Config, h Host, client *http.Client) (_ *Providers, err error) {
	if client == nil {
		return nil, errors.New("providers: shared HTTP client is required")
	}
	store, err := authStore(h)
	if err != nil {
		return nil, fmt.Errorf("providers: auth store: %w", err)
	}
	margin, err := time.ParseDuration(fmt.Sprintf("%ds", cfg.Auth.RefreshMarginSeconds))
	if err != nil {
		return nil, fmt.Errorf("providers: auth.refresh_margin_seconds: %w", err)
	}
	timeout, err := time.ParseDuration(fmt.Sprintf("%ds", cfg.Auth.TokenTimeoutSeconds))
	if err != nil {
		return nil, fmt.Errorf("providers: auth.token_timeout_seconds: %w", err)
	}
	credentials, err := auth.NewSource(auth.Options{
		Accounts: cfg.Accounts, Store: store, HTTPClient: client,
		Getenv: func(name string) string { return lookup(h.Environ, name) }, Flows: auth.Flows(),
		RefreshMargin: margin, TokenTimeout: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("providers: credentials: %w", err)
	}
	p := &Providers{Credentials: credentials, clients: make(map[string]provider.Provider)}
	defer func() {
		if err != nil {
			err = errors.Join(err, p.Close())
		}
	}()

	thinking, err := claudeThinking(cfg.Claude, func() models.Lists {
		if lists := p.lists.Load(); lists != nil {
			return *lists
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	claude, err := anthropic.New(anthropic.Config{
		HTTPClient: client, Version: version(), BaseURL: anthropic.DefaultBaseURL,
		Provider: anthropic.Provider, ErrorBodyBytes: anthropic.DefaultErrorBodyBytes,
		Thinking: thinking,
	})
	if err != nil {
		return nil, fmt.Errorf("providers: %s: %w", config.ProviderClaude, err)
	}
	p.clients[config.ProviderClaude] = claude
	responsesConfig := openai.Config{
		HTTPClient: client, BaseURL: openai.DefaultBaseURL, CodexBaseURL: openai.DefaultCodexBaseURL,
		Provider: openai.ProviderAPI, UserAgent: "towa/" + version(),
		Transport: openai.Transport(cfg.OpenAI.Transport), MaxIdleSockets: cfg.OpenAI.MaxIdleSockets,
		ParallelToolCalls: cfg.OpenAI.ParallelToolCalls,
		ErrorBodyBytes:    cfg.OpenAI.ErrorBodyBytes,
	}
	if cfg.Compaction.OpenAINative == config.NativeAuto {
		responsesConfig.CompactThreshold = cfg.Compaction.OpenAINativeThresholdTokens
	}
	// These fields have constructor fallbacks. Wiring must receive their
	// resolved values, not silently choose different settings here.
	if responsesConfig.Transport == "" {
		return nil, errors.New("providers: openai.transport is empty")
	}
	if responsesConfig.ErrorBodyBytes <= 0 {
		return nil, errors.New("providers: openai.error_body_bytes must be positive")
	}
	codex, err := openai.New(responsesConfig)
	if err != nil {
		return nil, fmt.Errorf("providers: %s: %w", config.ProviderCodex, err)
	}
	p.clients[config.ProviderCodex] = codex
	p.responses = append(p.responses, codex)
	for i, endpoint := range cfg.Providers {
		at := fmt.Sprintf("providers[%d]", i)
		if err := endpoint.Validate(at); err != nil {
			return nil, err
		}
		if _, exists := p.clients[endpoint.ID]; exists {
			return nil, fmt.Errorf("%s.id: %q already has a client", at, endpoint.ID)
		}
		switch endpoint.Kind {
		case config.ProviderChat:
			c, err := chat.New(chat.Config{HTTPClient: client, Provider: endpoint, UserAgent: "towa/" + version()})
			if err != nil {
				return nil, fmt.Errorf("%s: %w", at, err)
			}
			p.clients[endpoint.ID] = c
		case config.ProviderResponses:
			if endpoint.Referer != "" || endpoint.Title != "" || endpoint.CacheKeyField != "" {
				return nil, fmt.Errorf("%s: responses client does not support referer, title or cache_key_field", at)
			}
			for _, account := range cfg.Accounts {
				if account.Provider == endpoint.ID && (account.Kind == config.AccountOAuth || account.Kind == config.AccountProxiedOAuth) {
					return nil, fmt.Errorf("%s: custom responses endpoint %q requires API-key accounts, account %q is %s", at, endpoint.ID, account.ID, account.Kind)
				}
			}
			custom := responsesConfig
			custom.BaseURL, custom.Provider = endpoint.BaseURL, endpoint.ID
			c, err := openai.New(custom)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", at, err)
			}
			p.clients[endpoint.ID] = c
			p.responses = append(p.responses, c)
		}
	}
	return p, nil
}

// claudeThinking returns the thinking parameter of a request to each Claude
// model. A model the table or a provider's model list marks as without
// adaptive thinking is sent none: it answers the parameter with an HTTP 400.
// lists returns the model lists read so far.
func claudeThinking(cfg config.Claude, lists func() models.Lists) (func(model string) anthropic.Thinking, error) {
	var thinking anthropic.Thinking
	switch cfg.Thinking {
	case config.ClaudeThinkingAdaptive:
		thinking = anthropic.Thinking{Mode: anthropic.ThinkingAdaptive, Display: cfg.ThinkingDisplay}
	case config.ClaudeThinkingOff:
	default:
		return nil, fmt.Errorf("providers: claude.thinking %q is not known", cfg.Thinking)
	}
	return func(model string) anthropic.Thinking {
		if !lists().AdaptiveThinking(config.ProviderClaude, model) {
			return anthropic.Thinking{}
		}
		return thinking
	}, nil
}

// Client returns the client keyed by its configured provider id. Unknown
// ids stay unknown, so models.Model.Chain reports the missing dependency.
func (p *Providers) Client(id string) (provider.Provider, bool) {
	client, ok := p.clients[id]
	return client, ok
}

// Close closes the Responses clients' kept WebSocket connections. The
// caller owns the shared HTTP client and its idle connection pool.
func (p *Providers) Close() error {
	var errs []error
	for _, client := range p.responses {
		errs = append(errs, client.Close())
	}
	return errors.Join(errs...)
}
