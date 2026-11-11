package config

import (
	"fmt"
	"time"
)

// Events a hook can be attached to.
const (
	// HookPreTool runs before a tool call. It may deny the call or change
	// its input.
	HookPreTool = "pre_tool"
	// HookPostTool runs after a tool call, with its result.
	HookPostTool = "post_tool"
	// HookPrompt runs when a prompt arrives, before the model sees it.
	HookPrompt = "prompt"
	// HookStop runs when a turn is about to end.
	HookStop = "stop"
)

// Hook attaches a command to an event. src/hooks runs it and documents what
// the command is sent and what it may answer.
type Hook struct {
	// Event is pre_tool, post_tool, prompt or stop.
	Event string `json:"event" validate:"oneof=pre_tool post_tool prompt stop"`
	// Tools are the tool names, as the model calls them, that the hook
	// applies to. Empty means every tool. It must be empty for prompt and
	// stop, which have no tool.
	Tools []string `json:"tools" validate:"dive,required"`
	// Command is the program and its arguments. It is run as it is, with no
	// shell.
	Command []string `json:"command" validate:"min=1"`
	// Timeout is how long the command may run before it and everything it
	// started are killed, written as text such as 5s. It has no default:
	// hooks are a table, and a table's entry gets no defaults, so each hook
	// states its own.
	Timeout time.Duration `json:"timeout" validate:"gt=0"`
	// MaxOutputBytes is the most the command may write to its standard
	// output. More is a failed hook, not a cut-off answer. Zero means no
	// limit.
	MaxOutputBytes int64 `json:"max_output_bytes" validate:"gte=0"`
}

// validateHookList reports hooks that decode but cannot be run.
func validateHookList(hooks []Hook) error {
	for i, h := range hooks {
		if err := validateFields(fmt.Sprintf("hooks[%d]", i), h); err != nil {
			return err
		}
		switch h.Event {
		case HookPrompt, HookStop:
			if len(h.Tools) > 0 {
				return fmt.Errorf("hooks[%d].tools: a %s hook has no tool to match", i, h.Event)
			}
		}
		if h.Command[0] == "" {
			return fmt.Errorf("hooks[%d].command: the command is empty", i)
		}
	}
	return nil
}
