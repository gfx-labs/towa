package config

// MCP configures MCP servers. The package that reads it adds its fields,
// their defaults and their validation here.
type MCP struct{}

var mcpDefaults = map[string]any{}

func (MCP) validate() error { return nil }
