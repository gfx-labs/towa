package config

// Listen configures the unix socket and TCP listeners. The package that
// reads it adds its fields, their defaults and their validation here.
type Listen struct{}

var listenDefaults = map[string]any{}

func (Listen) validate() error { return nil }
