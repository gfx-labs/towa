package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider/anthropic"
)

// claude-haiku-4-5 answers the adaptive thinking parameter with an HTTP 400
// (docs/notes/2026-11-12-claude-thinking.md), so the default must not reach it.
func TestClaudeThinkingIsLeftOffAModelThatRefusesIt(t *testing.T) {
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config.Claude

	thinking, err := claudeThinking(cfg)
	require.NoError(t, err)
	require.Equal(t, anthropic.Thinking{Mode: anthropic.ThinkingAdaptive, Display: cfg.ThinkingDisplay}, thinking("claude-opus-5-5"))
	require.Equal(t, anthropic.Thinking{}, thinking("claude-haiku-4-5"))
	require.Equal(t, anthropic.Thinking{}, thinking("claude-haiku-4-5-20251001"))
	require.Equal(t, anthropic.ThinkingAdaptive, thinking("a-model-the-table-does-not-have").Mode)

	cfg.Thinking = config.ClaudeThinkingOff
	off, err := claudeThinking(cfg)
	require.NoError(t, err)
	require.Equal(t, anthropic.Thinking{}, off("claude-opus-5-5"))
}
