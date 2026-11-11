package config

// Store configures where sessions are kept. The package that reads it adds
// its fields, their defaults and their validation here.
type Store struct{}

var storeDefaults = map[string]any{}

func (Store) validate() error { return nil }
