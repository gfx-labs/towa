// Package config resolves acpharness's configuration from its layers into
// one typed struct. The rest of the program reads only that struct.
//
// A key is a field with kong tags: its name, its default and its help. A
// section is an embedded struct whose prefix is the section's name and a
// dot. Limits.MaxWall is the key limits.max_wall: the flag
// --limits.max_wall, the variable ACPHARNESS_LIMITS__MAX_WALL, and max_wall
// under limits in a file. A key of type "table" holds a list or a map of
// structs. kong has no flags for the fields of those structs, so they name
// their keys with json tags and encoding/json/v2 decodes them.
package config

import (
	"fmt"
	"log/slog"
)

// Config is the resolved configuration. Each section has its own file, with
// its type, its defaults and its validation. A field is added to a section
// when the code that reads it exists. Every key the program reads has a
// default in its tag, so a resolved Config never holds a zero value by
// accident. A table, such as accounts, has no default.
type Config struct {
	// DefaultModel names the entry of Models a session starts with.
	DefaultModel string `name:"default_model" group:"models" help:"Name of the model a session starts with."`
	// Models are the named models, each an ordered chain of accounts.
	Models             map[string]Model   `name:"models" type:"table" group:"models" placeholder:"YAML" help:"Named models: chain (provider:account/model entries), then optionally effort, context_window and max_output_tokens. Written in a config file."`
	Telemetry          Telemetry          `embed:"" prefix:"telemetry." group:"telemetry"`
	Auth               Auth               `embed:"" prefix:"auth." group:"auth"`
	Accounts           []Account          `name:"accounts" type:"table" group:"accounts" placeholder:"YAML" help:"Accounts a proxy holds or an API key backs: id, provider, kind. A login makes its own. Written in a config file."`
	Providers          []Provider         `name:"providers" type:"table" group:"providers" placeholder:"YAML" help:"Endpoints that are not built in: id, kind, base_url. Written in a config file."`
	Store              Store              `embed:"" prefix:"store." group:"store"`
	Tools              Tools              `embed:"" prefix:"tools." group:"tools"`
	MCP                MCP                `embed:"" prefix:"mcp." group:"mcp"`
	Session            Session            `embed:"" prefix:"session." group:"session"`
	Context            Context            `embed:"" prefix:"context." group:"context"`
	Listen             Listen             `embed:"" prefix:"listen." group:"listen"`
	Limits             Limits             `embed:"" prefix:"limits." group:"limits"`
	Hooks              []Hook             `name:"hooks" type:"table" group:"hooks" placeholder:"YAML" help:"Commands run on an event: event, command, timeout. Written in a config file."`
	Embeddings         Embeddings         `embed:"" prefix:"embeddings." group:"embeddings"`
	OpenAI             OpenAI             `embed:"" prefix:"openai." group:"openai"`
	WebFetch           WebFetch           `embed:"" prefix:"webfetch." group:"webfetch"`
	WebSearch          WebSearch          `embed:"" prefix:"websearch." group:"websearch"`
	Jev                Jev                `embed:"" prefix:"jev." group:"jev"`
	Compaction         Compaction         `embed:"" prefix:"compaction." group:"compaction"`
	ConversationSearch ConversationSearch `embed:"" prefix:"conversation_search." group:"conversation_search"`
	Subagents          Subagents          `embed:"" prefix:"subagents." group:"subagents"`
}

// Telemetry configures logs and OpenTelemetry.
type Telemetry struct {
	// OTel turns on the export of traces, metrics and logs over OTLP/HTTP.
	// The endpoint and headers come from the standard OTEL_EXPORTER_OTLP_*
	// environment variables.
	OTel bool `name:"otel" help:"Export traces, metrics and logs over OTLP/HTTP, to the endpoint the OTEL_EXPORTER_OTLP_* variables name."`
	Logs Logs `embed:"" prefix:"logs."`
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
	if err := validateFields("telemetry", c.Telemetry); err != nil {
		return err
	}
	for _, section := range []func() error{
		c.Auth.validate, c.validateAccounts, c.validateProviders, c.Store.validate, c.Tools.validate, c.MCP.validate,
		c.Session.validate, c.Context.validate, c.Listen.validate, c.Limits.validate,
		func() error { return validateHookList(c.Hooks) },
		c.Embeddings.validate, c.validateEmbeddingsAccount, c.OpenAI.validate, c.WebFetch.validate, c.WebSearch.validate,
		c.Jev.validate, c.validateJevAccount, c.Compaction.validate, c.validateCompaction,
		c.ConversationSearch.validate, c.Subagents.validate, c.validateSubagentModels,
		func() error { return validateModelSet(c.DefaultModel, c.Models, c.Accounts, c.Providers) },
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
		if _, ok := c.Models[name]; !ok {
			return fmt.Errorf("subagents.models: %q is not a configured model", name)
		}
	}
	return nil
}

func (c *Config) validateCompaction() error {
	if c.Compaction.Mode == CompactionSemantic && c.Embeddings.BaseURL == "" {
		return fmt.Errorf("compaction.mode %q requires embeddings.base_url", CompactionSemantic)
	}
	if c.OpenAI.CompactThreshold != 0 {
		return fmt.Errorf("openai.compact_threshold is no longer supported; use compaction.openai_native_threshold_tokens with compaction.openai_native")
	}
	return nil
}
