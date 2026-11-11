package config

// Limits configures what one prompt turn may use: tool calls, wall time,
// tokens, subagent depth and concurrency. The package that reads it adds its
// fields, their defaults and their validation here.
type Limits struct{}

var limitsDefaults = map[string]any{}

func (Limits) validate() error { return nil }
