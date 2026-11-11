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
	Event string `koanf:"event"`
	// Tools are the tool names, as the model calls them, that the hook
	// applies to. Empty means every tool. It must be empty for prompt and
	// stop, which have no tool.
	Tools []string `koanf:"tools"`
	// Command is the program and its arguments. It is run as it is, with no
	// shell.
	Command []string `koanf:"command"`
	// Timeout is how long the command may run before it and everything it
	// started are killed. It has no default: hooks are a list, and a list
	// entry gets no defaults, so each hook states its own.
	Timeout time.Duration `koanf:"timeout"`
	// MaxOutputBytes is the most the command may write to its standard
	// output. More is a failed hook, not a cut-off answer. Zero means no
	// limit.
	MaxOutputBytes int64 `koanf:"max_output_bytes"`
}

// validateHookList reports hooks that decode but cannot be run.
func validateHookList(hooks []Hook) error {
	for i, h := range hooks {
		switch h.Event {
		case HookPreTool, HookPostTool:
		case HookPrompt, HookStop:
			if len(h.Tools) > 0 {
				return fmt.Errorf("hooks[%d].tools: a %s hook has no tool to match", i, h.Event)
			}
		default:
			return fmt.Errorf("hooks[%d].event: %q is not %s, %s, %s or %s",
				i, h.Event, HookPreTool, HookPostTool, HookPrompt, HookStop)
		}
		for j, name := range h.Tools {
			if name == "" {
				return fmt.Errorf("hooks[%d].tools[%d]: the name is empty", i, j)
			}
		}
		if len(h.Command) == 0 || h.Command[0] == "" {
			return fmt.Errorf("hooks[%d].command: the command is empty", i)
		}
		if h.Timeout <= 0 {
			return fmt.Errorf("hooks[%d].timeout: must be greater than zero", i)
		}
		if h.MaxOutputBytes < 0 {
			return fmt.Errorf("hooks[%d].max_output_bytes: %d is negative", i, h.MaxOutputBytes)
		}
	}
	return nil
}
