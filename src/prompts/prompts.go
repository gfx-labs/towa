// Package prompts supplies pinned upstream captures and authored generic instructions.
package prompts

import "fmt"

//go:generate go run ../../tools/capture/prompts.go -root ../..

// Prompt holds instruction text, not the provider's tool schemas.
type Prompt struct {
	System       string
	Tools        string
	Interruption string
	// InterruptionRole is the instruction message role, either user or developer.
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

// Lookup returns authored generic instructions or exact captured instructions.
// Callers validate model metadata through models before preparing a request.
// Codex uses
// instructions_template without persistent mode or feature overrides, with
// interruption messages enabled. Model names are not normalized or aliased.
// Claude prompts are unavailable until their captures are deployed.
func Lookup(dialect, model string) (Prompt, error) {
	if dialect == "generic" {
		if model == "" {
			return Prompt{}, &LookupError{Dialect: dialect, Model: model, Reason: "wire model is empty"}
		}
		return Prompt{
			System:           "You are acpharness, a coding assistant. Follow the user's instructions and the project rules provided in context. Inspect relevant files before changing them. Make the smallest complete change and check the affected behavior. Report what changed, what was checked, and any unresolved limitations. Do not claim to have run commands or verified results that you have not observed.",
			Tools:            "Use the supplied tool definitions to inspect files, make changes, and run checks. Respect tool errors and permission decisions. Do not assume a failed or interrupted tool call made no changes. Inspect the current state before retrying an operation whose outcome is uncertain.",
			Interruption:     "The previous turn was interrupted. Running operations may have stopped after making partial changes. Check the current state before resuming or retrying them.",
			InterruptionRole: "user",
		}, nil
	}
	if dialect != "codex" {
		return Prompt{}, &LookupError{Dialect: dialect, Model: model, Reason: "dialect has no deployed capture"}
	}
	prompt, ok := codexPrompts[model]
	if !ok {
		return Prompt{}, &LookupError{Dialect: dialect, Model: model, Reason: "exact model variant is not captured at Codex " + codexCommit}
	}
	return prompt, nil
}
