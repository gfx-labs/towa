package config

// Context configures what is loaded into a session's system prompt: agent
// instruction files, skills, extra files and directories. The package that
// reads it adds its fields, their defaults and their validation here.
type Context struct{}

var contextDefaults = map[string]any{}

func (Context) validate() error { return nil }
