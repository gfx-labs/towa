// Package config loads acpharness's configuration from its layers and
// decodes it into one typed struct. The rest of the program reads only that
// struct.
package config

import (
	"fmt"
	"log/slog"
	"maps"
)

// Config is the resolved configuration. Each section has its own file, with
// its type, its defaults and its validation. A field is added to a section
// when the code that reads it exists.
type Config struct {
	// DefaultModel names the entry of Models a session starts with.
	DefaultModel string `koanf:"default_model"`
	// Models are the named models, each an ordered chain of accounts.
	Models             map[string]Model   `koanf:"models"`
	Telemetry          Telemetry          `koanf:"telemetry"`
	Auth               Auth               `koanf:"auth"`
	Accounts           []Account          `koanf:"accounts"`
	Providers          []Provider         `koanf:"providers"`
	Store              Store              `koanf:"store"`
	Tools              Tools              `koanf:"tools"`
	MCP                MCP                `koanf:"mcp"`
	Session            Session            `koanf:"session"`
	Context            Context            `koanf:"context"`
	Listen             Listen             `koanf:"listen"`
	Limits             Limits             `koanf:"limits"`
	Hooks              []Hook             `koanf:"hooks"`
	Embeddings         Embeddings         `koanf:"embeddings"`
	OpenAI             OpenAI             `koanf:"openai"`
	WebFetch           WebFetch           `koanf:"webfetch"`
	WebSearch          WebSearch          `koanf:"websearch"`
	Jev                Jev                `koanf:"jev"`
	Compaction         Compaction         `koanf:"compaction"`
	ConversationSearch ConversationSearch `koanf:"conversation_search"`
	Subagents          Subagents          `koanf:"subagents"`
}

// Telemetry configures logs and OpenTelemetry.
type Telemetry struct {
	// OTel turns on the export of traces, metrics and logs over OTLP/HTTP.
	// The endpoint and headers come from the standard OTEL_EXPORTER_OTLP_*
	// environment variables.
	OTel bool `koanf:"otel"`
	Logs Logs `koanf:"logs"`
}

// Logs configures the two log destinations separately.
type Logs struct {
	Stderr StderrLog `koanf:"stderr"`
	OTel   OTelLog   `koanf:"otel"`
}

// StderrLog configures the log written to standard error.
type StderrLog struct {
	Enabled bool       `koanf:"enabled"`
	Level   slog.Level `koanf:"level"`
	Format  string     `koanf:"format" validate:"oneof=text json"`
}

// Log formats for StderrLog.Format.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// OTelLog configures the log exported over OTLP. It applies only when
// Telemetry.OTel is on.
type OTelLog struct {
	Enabled bool       `koanf:"enabled"`
	Level   slog.Level `koanf:"level"`
}

// telemetryDefaults holds the defaults of the telemetry section.
var telemetryDefaults = map[string]any{
	"telemetry.otel":                false,
	"telemetry.logs.stderr.enabled": true,
	"telemetry.logs.stderr.level":   "info",
	"telemetry.logs.stderr.format":  FormatText,
	"telemetry.logs.otel.enabled":   true,
	"telemetry.logs.otel.level":     "info",
}

// defaults is the first layer: every section's defaults. Every key the
// program reads has a value here, so a decoded Config never holds a zero
// value by accident. A list, such as accounts, has no default.
func defaults() map[string]any {
	all := map[string]any{}
	for _, section := range []map[string]any{
		telemetryDefaults, storeDefaults, toolsDefaults, mcpDefaults, sessionDefaults, contextDefaults, listenDefaults,
		limitsDefaults, authDefaults, embeddingsDefaults, openaiDefaults, webfetchDefaults, websearchDefaults, jevDefaults, compactionDefaults,
		conversationSearchDefaults, subagentsDefaults,
	} {
		maps.Copy(all, section)
	}
	return all
}

// FlagKeys maps a command-line flag name to the config key it sets.
var FlagKeys = map[string]string{
	"log-level":  "telemetry.logs.stderr.level",
	"log-format": "telemetry.logs.stderr.format",
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
