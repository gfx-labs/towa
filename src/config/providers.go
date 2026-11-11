package config

import (
	"fmt"
	"net/url"
)

// Provider is an endpoint that is not built in, such as an OpenAI-compatible
// API. The package that reads it adds the fields it needs here.
type Provider struct {
	// ID names the provider in accounts and model chains.
	ID   string `koanf:"id"`
	Kind string `koanf:"kind"`
	// BaseURL is the endpoint's root, such as https://openrouter.ai/api/v1.
	BaseURL string `koanf:"base_url"`
}

// Kinds of a provider: the wire protocol it speaks.
const (
	ProviderChat      = "chat"
	ProviderResponses = "responses"
)

func (c *Config) validateProviders() error {
	seen := map[string]bool{}
	for i, p := range c.Providers {
		at := fmt.Sprintf("providers[%d]", i)
		switch {
		case p.ID == "":
			return fmt.Errorf("%s.id is empty", at)
		case seen[p.ID]:
			return fmt.Errorf("%s.id: %q is used by more than one provider", at, p.ID)
		case p.Kind != ProviderChat && p.Kind != ProviderResponses:
			return fmt.Errorf("%s.kind: %q is not %q or %q", at, p.Kind, ProviderChat, ProviderResponses)
		}
		seen[p.ID] = true
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("%s.base_url: %q is not an http or https URL", at, p.BaseURL)
		}
	}
	return nil
}
