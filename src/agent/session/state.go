package session

import (
	"encoding/json/v2"
	"fmt"
	"maps"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/harness"
	"github.com/gfx-labs/towa/src/store"
)

// savedState holds session settings not represented by the store's named
// fields. MCP servers are supplied by each attach, never stored here.
type savedState struct {
	Values       map[acp.SessionConfigId]Value `json:"values"`
	Overrides    map[string]any                `json:"overrides"`
	SystemPrompt *acp.SystemPrompt             `json:"systemPrompt,omitzero"`
}

func (a *Agent) restore(meta store.Meta, request map[string]any, prompt *acp.SystemPrompt) (savedState, map[string]any, *acp.SystemPrompt, error) {
	var state savedState
	if len(meta.State) > 0 {
		if err := json.Unmarshal(meta.State, &state, json.RejectUnknownMembers(true)); err != nil {
			return state, nil, nil, fmt.Errorf("session: decoding stored state: %w", err)
		}
	}
	// The named fields remain authoritative for old and new stores alike.
	if state.Values == nil {
		state.Values = map[acp.SessionConfigId]Value{}
	}
	maps.Copy(state.Values, a.storedValues(meta))
	if prompt == nil {
		prompt = state.SystemPrompt
	}
	if err := a.checkSystemPrompt(prompt); err != nil {
		return state, nil, nil, err
	}
	merged := maps.Clone(request)
	if merged == nil {
		merged = map[string]any{}
	}
	overrides := maps.Clone(state.Overrides)
	if overrides == nil {
		overrides = map[string]any{}
	}
	if raw, ok := request[harness.Key]; ok {
		incoming, ok := raw.(map[string]any)
		if !ok {
			return state, nil, nil, invalid("_meta.%s is not an object", harness.Key)
		}
		maps.Copy(overrides, incoming)
	}
	if len(overrides) > 0 {
		merged[harness.Key] = overrides
	}
	return state, merged, prompt, nil
}

func overrides(meta map[string]any) map[string]any {
	raw, _ := meta[harness.Key].(map[string]any)
	return maps.Clone(raw)
}
