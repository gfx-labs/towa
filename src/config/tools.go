package config

// Tools configures the tools a model can call. The package that reads it
// adds its fields, their defaults and their validation here.
type Tools struct{}

var toolsDefaults = map[string]any{}

func (Tools) validate() error { return nil }
