package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/config/configtest"
	"github.com/gfx-labs/towa/src/provider/anthropic"
	"github.com/gfx-labs/towa/src/provider/models"
)

// claude-haiku-4-5 answers the adaptive thinking parameter with an HTTP 400,
// so the default must not reach it.
func TestClaudeThinkingIsLeftOffAModelThatRefusesIt(t *testing.T) {
	loaded, err := configtest.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config.Claude

	// What Anthropic's list said of these two on 2026-11-12.
	lists := models.Lists{"claude:work": {Models: map[string]models.Listed{
		"claude-opus-4-5-20251101":  {ContextWindow: 200_000, MaxOutputTokens: 64_000, NoAdaptiveThinking: true},
		"claude-haiku-4-5-20251001": {ContextWindow: 200_000, MaxOutputTokens: 64_000, NoAdaptiveThinking: true},
	}}}
	thinking, err := claudeThinking(cfg, func() models.Lists { return lists })
	require.NoError(t, err)
	require.Equal(t, anthropic.Thinking{Mode: anthropic.ThinkingAdaptive, Display: cfg.ThinkingDisplay}, thinking("claude-opus-5-5"))
	require.Equal(t, anthropic.Thinking{}, thinking("claude-haiku-4-5"))
	require.Equal(t, anthropic.Thinking{}, thinking("claude-haiku-4-5-20251001"))
	require.Equal(t, anthropic.Thinking{}, thinking("claude-opus-4-5"))
	require.Equal(t, anthropic.ThinkingAdaptive, thinking("a-model-the-table-does-not-have").Mode)

	cfg.Thinking = config.ClaudeThinkingOff
	off, err := claudeThinking(cfg, func() models.Lists { return nil })
	require.NoError(t, err)
	require.Equal(t, anthropic.Thinking{}, off("claude-opus-5-5"))
}
