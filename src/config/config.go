// Package config loads acpharness's configuration from its layers and
// decodes it into one typed struct. The rest of the program reads only that
// struct.
package config

import (
	"fmt"
	"log/slog"
)

// Config is the resolved configuration. A section is added here when the
// code that reads it exists.
type Config struct {
	Telemetry Telemetry `koanf:"telemetry"`
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
	Format  string     `koanf:"format"`
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

// defaults is the first layer. Every key the program reads has a value here,
// so a decoded Config never holds a zero value by accident.
var defaults = map[string]any{
	"telemetry.otel":                false,
	"telemetry.logs.stderr.enabled": true,
	"telemetry.logs.stderr.level":   "info",
	"telemetry.logs.stderr.format":  FormatText,
	"telemetry.logs.otel.enabled":   true,
	"telemetry.logs.otel.level":     "info",
}

// FlagKeys maps a command-line flag name to the config key it sets.
var FlagKeys = map[string]string{
	"log-level":  "telemetry.logs.stderr.level",
	"log-format": "telemetry.logs.stderr.format",
}

// validate reports values that decode but that the program cannot act on.
func (c *Config) validate() error {
	switch f := c.Telemetry.Logs.Stderr.Format; f {
	case FormatText, FormatJSON:
	default:
		return fmt.Errorf("telemetry.logs.stderr.format: %q is not %q or %q", f, FormatText, FormatJSON)
	}
	return nil
}
