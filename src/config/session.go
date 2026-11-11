package config

// Session configures what a new session starts with. The package that reads
// it adds its fields, their defaults and their validation here.
type Session struct{}

var sessionDefaults = map[string]any{}

func (Session) validate() error { return nil }
