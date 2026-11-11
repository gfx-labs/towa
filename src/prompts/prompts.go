// Package prompts deploys the pinned upstream instruction captures.
package prompts

import "fmt"

//go:generate go run ../../tools/capture/prompts.go -root ../..

// Prompt holds instruction text, not the provider's tool schemas.
type Prompt struct {
	System       string
	Tools        string
	Interruption string
	// InterruptionRole is the captured message role, either user or developer.
	InterruptionRole string
}

// LookupError reports an unavailable dialect or exact captured model variant.
type LookupError struct {
	Dialect string
	Model   string
	Reason  string
}

func (e *LookupError) Error() string {
	return fmt.Sprintf("prompts: dialect %q model %q: %s", e.Dialect, e.Model, e.Reason)
}

// Lookup returns the exact catalog model's captured instructions. Codex uses
// instructions_template without persistent mode or feature overrides, with
// interruption messages enabled. Model names are not normalized or aliased.
// Claude and generic prompts are unavailable until their captures are deployed.
func Lookup(dialect, model string) (Prompt, error) {
	if dialect != "codex" {
		return Prompt{}, &LookupError{Dialect: dialect, Model: model, Reason: "dialect has no deployed capture"}
	}
	prompt, ok := codexPrompts[model]
	if !ok {
		return Prompt{}, &LookupError{Dialect: dialect, Model: model, Reason: "exact model variant is not captured at Codex " + codexCommit}
	}
	return prompt, nil
}
