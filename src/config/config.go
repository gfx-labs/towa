// Package config resolves towa's configuration from its layers into
// one typed struct. The rest of the program reads only that struct.
//
// A key is a field with kong tags: its name, its default and its help. A
// section is an embedded struct whose prefix is the section's name and a
// dot. Limits.MaxWall is the key limits.max_wall: the flag
// --limits.max_wall, the variable TOWA_LIMITS__MAX_WALL, and max_wall
// under limits in a file. A key of type "table" holds a list or a map of
// structs. kong has no flags for the fields of those structs, so they name
// their keys with json tags and encoding/json/v2 decodes them.
package config

import (
	"fmt"
	"log/slog"
	"slices"
)

// Config keys of the figures of a model the built-in table does not have.
// A model that is refused for lacking one names it.
const (
	KeyModelContextWindow   = "model_context_window"
	KeyModelMaxOutputTokens = "model_max_output_tokens"
)

// Config is the resolved configuration. Each section has its own file, with
// its type, its defaults and its validation. A field is added to a section
// when the code that reads it exists. Every key the program reads has a
// default in its tag, so a resolved Config never holds a zero value by
// accident. A table, such as accounts, has no default.
type Config struct {
	// Models are the models a session can select, each "provider:account/model"
	// with an optional "#effort". A session starts with the first. A request
	// that an entry cannot take falls back to the entries listed after it.
	Models []string `name:"models" group:"models" placeholder:"MODEL" help:"Models a session can select, in order: provider:account/model, optionally with #effort (claude:work/claude-opus-5-5#medium). provider:model means every account of the provider. Comma separated as a flag, a list in a file. A session starts with the first, and a request an entry cannot take falls back to the entries after it. Without #effort none is sent and the provider's default applies."`
	// ModelContextWindow and ModelMaxOutputTokens are the figures of a model the
	// built-in table in src/provider/models does not have and no provider's models
	// entry gives. Zero is not set: the figure is then taken from the
	// provider's model list, then guessed from the model's name, and a model
	// neither says anything about is refused when it is selected.
	ModelContextWindow   int64 `name:"model_context_window" group:"models" default:"0" validate:"gte=0" help:"Context size in tokens of a model the built-in table does not know, for every such model at once: providers[].models gives it for one model. 0 is not set: the figure then comes from the provider's model list, which is where every Claude model's figures come from, or for a newer version of a model family the table has (gpt-7-sol) from that family, and any other unknown model is refused."`
	ModelMaxOutputTokens int64 `name:"model_max_output_tokens" group:"models" default:"0" validate:"gte=0" help:"Most tokens one response may hold, for a model the built-in table does not know, for every such model at once: providers[].models gives it for one model. 0 is not set: the figure then comes from the provider's model list, or for a newer version of a model family the table has from that family, and any other unknown model is refused."`
	// ModelListMaxAgeSeconds is how long a provider's model list is kept
	// before it is read again. jcode keeps one for 30 minutes.
	ModelListMaxAgeSeconds int64 `name:"model_list_max_age_seconds" group:"models" default:"1800" validate:"gte=0" help:"Seconds a provider's model list is kept in models.json, in the state directory, before a start reads it again. It is read for a configured model the built-in table does not know, which is every Claude model. 0 reads it at every start."`
	// ModelListTimeoutSeconds bounds reading one account's model list.
	ModelListTimeoutSeconds int64              `name:"model_list_timeout_seconds" group:"models" default:"30" validate:"gt=0" help:"Seconds reading one account's model list from its provider may take at start. A list that is not read in time is logged and the model's name is used."`
	Telemetry               Telemetry          `embed:"" prefix:"telemetry." group:"telemetry"`
	Auth                    Auth               `embed:"" prefix:"auth." group:"auth"`
	Accounts                []Account          `name:"accounts" type:"table" group:"accounts" placeholder:"YAML" help:"Accounts without a stored login: one a proxy holds, one an API key backs, or one whose endpoint takes no credential (kind none). id, provider, kind. Written in a config file."`
	Providers               []Provider         `name:"providers" type:"table" group:"providers" placeholder:"YAML" help:"Endpoints that are not built in: id, kind, base_url, and models, which gives the context_window and max_output_tokens of each model id. Written in a config file."`
	Store                   Store              `embed:"" prefix:"store." group:"store"`
	Tools                   Tools              `embed:"" prefix:"tools." group:"tools"`
	MCP                     MCP                `embed:"" prefix:"mcp." group:"mcp"`
	Session                 Session            `embed:"" prefix:"session." group:"session"`
	Context                 Context            `embed:"" prefix:"context." group:"context"`
	Listen                  Listen             `embed:"" prefix:"listen." group:"listen"`
	Limits                  Limits             `embed:"" prefix:"limits." group:"limits"`
	Hooks                   []Hook             `name:"hooks" type:"table" group:"hooks" placeholder:"YAML" help:"Commands run on an event: event, command, timeout. Written in a config file."`
	Embeddings              Embeddings         `embed:"" prefix:"embeddings." group:"embeddings"`
	OpenAI                  OpenAI             `embed:"" prefix:"openai." group:"openai"`
	Claude                  Claude             `embed:"" prefix:"claude." group:"claude"`
	WebFetch                WebFetch           `embed:"" prefix:"webfetch." group:"webfetch"`
	WebSearch               WebSearch          `embed:"" prefix:"websearch." group:"websearch"`
	Jev                     Jev                `embed:"" prefix:"jev." group:"jev"`
	Compaction              Compaction         `embed:"" prefix:"compaction." group:"compaction"`
	ConversationSearch      ConversationSearch `embed:"" prefix:"conversation_search." group:"conversation_search"`
	Subagents               Subagents          `embed:"" prefix:"subagents." group:"subagents"`
}

// Telemetry configures logs and OpenTelemetry.
type Telemetry struct {
	// OTel turns on the export of traces, metrics and logs over OTLP/HTTP.
	// The endpoint and headers come from the standard OTEL_EXPORTER_OTLP_*
	// environment variables.
	OTel  bool  `name:"otel" help:"Export traces, metrics and logs over OTLP/HTTP, to the endpoint the OTEL_EXPORTER_OTLP_* variables name."`
	Logs  Logs  `embed:"" prefix:"logs."`
	Pprof Pprof `embed:"" prefix:"pprof."`
}

// Pprof configures Go's profiling pages. They are on when Address is set.
type Pprof struct {
	// Address is host:port on a loopback address. The pages have no
	// authentication and show the program's memory.
	Address string `name:"address" placeholder:"HOST:PORT" help:"Loopback address to serve Go's profiling pages on, under /debug/pprof/. Port 0 takes a free port, which the log names. Empty means no profiling pages."`
}

func (p Pprof) validate() error {
	if p.Address == "" {
		return nil
	}
	if !(ListenTCP{Address: p.Address}).Loopback() {
		return fmt.Errorf("telemetry.pprof.address: %q is not host:port on a loopback address", p.Address)
	}
	return nil
}

// Logs configures the two log destinations separately.
type Logs struct {
	Stderr StderrLog `embed:"" prefix:"stderr."`
	OTel   OTelLog   `embed:"" prefix:"otel."`
}

// StderrLog configures the log written to standard error.
type StderrLog struct {
	Enabled bool       `name:"enabled" default:"true" help:"Write the log to standard error. On by default."`
	Level   slog.Level `name:"level" aliases:"log-level" default:"info" help:"Lowest level written to standard error: debug, info, warn or error. Also --log-level."`
	Format  string     `name:"format" aliases:"log-format" default:"text" validate:"oneof=text json" help:"Format of the log on standard error: text or json. Also --log-format."`
}

// Log formats for StderrLog.Format.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// OTelLog configures the log exported over OTLP. It applies only when
// Telemetry.OTel is on.
type OTelLog struct {
	Enabled bool       `name:"enabled" default:"true" help:"Export the log over OTLP when telemetry.otel is on. On by default."`
	Level   slog.Level `name:"level" default:"info" help:"Lowest level exported over OTLP: debug, info, warn or error."`
}

// validate reports values that decode but that the program cannot act on.
func (c *Config) validate() error {
	for key, value := range map[string]int64{KeyModelContextWindow: c.ModelContextWindow, KeyModelMaxOutputTokens: c.ModelMaxOutputTokens, "model_list_max_age_seconds": c.ModelListMaxAgeSeconds} {
		if value < 0 {
			return fmt.Errorf("%s: %d is negative", key, value)
		}
	}
	if c.ModelListTimeoutSeconds <= 0 {
		return fmt.Errorf("model_list_timeout_seconds: %d is not positive", c.ModelListTimeoutSeconds)
	}
	if err := validateFields("telemetry", c.Telemetry); err != nil {
		return err
	}
	if err := c.Telemetry.Pprof.validate(); err != nil {
		return err
	}
	for _, section := range []func() error{
		c.Auth.validate, c.validateAccounts, c.validateProviders, c.Store.validate, c.Tools.validate, c.MCP.validate,
		c.Session.validate, c.Context.validate, c.Listen.validate, c.Limits.validate,
		func() error { return validateHookList(c.Hooks) },
		c.Embeddings.validate, c.validateEmbeddingsAccount, c.OpenAI.validate, c.Claude.validate, c.WebFetch.validate, c.WebSearch.validate,
		c.Jev.validate, c.validateJevAccount, c.Compaction.validate, c.validateCompaction,
		c.ConversationSearch.validate, c.Subagents.validate, c.validateSubagentModels,
		func() error { return validateModels(c.Models, c.Accounts, c.Providers) },
	} {
		if err := section(); err != nil {
			return err
		}
	}
	return nil
}

// validateEmbeddingsAccount reports an embeddings account that no account
// entry defines. An empty account means the endpoint takes no credential.
func (c *Config) validateEmbeddingsAccount() error {
	if c.Embeddings.Account == "" {
		return nil
	}
	for _, a := range c.Accounts {
		if a.ID == c.Embeddings.Account {
			return nil
		}
	}
	return fmt.Errorf("embeddings.account: %q is not the id of a configured account", c.Embeddings.Account)
}

// validateJevAccount reports a Jev account that no account entry defines.
// Whether a route may leave the account empty is the section's own rule.
func (c *Config) validateJevAccount() error {
	if c.Jev.Account == "" {
		return nil
	}
	for _, a := range c.Accounts {
		if a.ID == c.Jev.Account {
			return nil
		}
	}
	return fmt.Errorf("jev.account: %q is not the id of a configured account", c.Jev.Account)
}

func (c *Config) validateSubagentModels() error {
	for _, name := range c.Subagents.Models {
		if !slices.Contains(c.Models, name) {
			return fmt.Errorf("subagents.models: %q is not a configured model", name)
		}
	}
	return nil
}

func (c *Config) validateCompaction() error {
	if c.Compaction.Mode == CompactionSemantic && c.Embeddings.BaseURL == "" {
		return fmt.Errorf("compaction.mode %q requires embeddings.base_url", CompactionSemantic)
	}
	return nil
}
