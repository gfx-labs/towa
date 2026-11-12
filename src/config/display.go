package config

import (
	"encoding/json/v2"
	"maps"
	"net/url"
	"slices"
	"strings"
)

const redacted = "[redacted]"

// displayValue leaves credentials in Config and removes them from diagnostics.
func displayValue(key string, value any) ([]byte, error) {
	switch v := value.(type) {
	case []MCPServer:
		servers := slices.Clone(v)
		for i := range servers {
			s := &servers[i]
			s.URL = redactURL(s.URL)
			s.Headers = redactMap(s.Headers)
			s.Env = redactMap(s.Env)
			s.Args = redactArgs(s.Args)
		}
		value = servers
	case []Provider:
		providers := slices.Clone(v)
		for i := range providers {
			providers[i].BaseURL = redactURL(providers[i].BaseURL)
			providers[i].Referer = redactURL(providers[i].Referer)
		}
		value = providers
	case []Hook:
		hooks := slices.Clone(v)
		for i := range hooks {
			command := slices.Clone(hooks[i].Command)
			for j := 1; j < len(command); j++ {
				command[j] = redacted
			}
			hooks[i].Command = command
		}
		value = hooks
	case string:
		if strings.HasSuffix(key, ".base_url") || key == "websearch.endpoint" || key == "jev.referer" {
			value = redactURL(v)
		}
	}
	return json.Marshal(value, valueOptions)
}

func redactMap(values map[string]string) map[string]string {
	out := maps.Clone(values)
	for key := range out {
		out[key] = redacted
	}
	return out
}

func redactArgs(args []string) []string {
	out := slices.Clone(args)
	for i := range out {
		out[i] = redacted
	}
	return out
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return redacted
	}
	if u.User != nil {
		u.User = url.User(redacted)
	}
	if u.RawQuery != "" {
		u.RawQuery = redacted
	}
	if u.Fragment != "" {
		u.Fragment, u.RawFragment = redacted, ""
	}
	return u.String()
}
