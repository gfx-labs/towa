package prompts_test

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/prompts"
)

// The canonical capture is independent of the generated deployment. This
// catches stale deployments, changed bytes and incorrect model/role selection.
func TestLookupCapturedCodex(t *testing.T) {
	base := filepath.Join("..", "..", "prompts", "codex", "ce0323f2")
	catalog, err := os.ReadFile(filepath.Join(base, "source", "codex-rs", "models-manager", "models.json"))
	require.NoError(t, err)
	var metadata struct {
		Models []struct {
			Slug              string  `json:"slug"`
			MultiAgentVersion *string `json:"multi_agent_version"`
			Messages          struct {
				Template string `json:"instructions_template"`
				Tools    any    `json:"tools"`
			} `json:"model_messages"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(catalog, &metadata))
	require.NotEmpty(t, metadata.Models)
	manifestBytes, err := os.ReadFile(filepath.Join(base, "manifest.json"))
	require.NoError(t, err)
	var manifest struct {
		Interruption map[string]struct {
			File string `json:"file"`
			Role string `json:"role"`
		} `json:"interruption"`
	}
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	for _, model := range metadata.Models {
		t.Run(model.Slug, func(t *testing.T) {
			got, err := prompts.Lookup("codex", model.Slug)
			require.NoError(t, err)
			assert.Equal(t, model.Messages.Template, got.System)
			require.Nil(t, model.Messages.Tools, "a tools message needs its own captured text mapping")
			assert.Empty(t, got.Tools)
			// Captured core/src/tasks/mod.rs:85-98 selects developer only for v2.
			variant := "contextual_user"
			if model.MultiAgentVersion != nil && *model.MultiAgentVersion == "v2" {
				variant = "developer"
			}
			marker, ok := manifest.Interruption[variant]
			require.True(t, ok)
			text, err := os.ReadFile(filepath.Join(base, marker.File))
			require.NoError(t, err)
			assert.Equal(t, string(text), got.Interruption)
			assert.Equal(t, marker.Role, got.InterruptionRole)
		})
	}
	for _, input := range []struct{ dialect, model string }{
		{"generic", "gpt-6.1-sol"},
		{"claude", "claude-opus-5-5"},
		{"codex", ""},
		{"codex", "unknown-model"},
		{"codex", "openai/gpt-6.1-sol"},
		{"codex", "gpt-6.1-sol/persistent"},
	} {
		_, err := prompts.Lookup(input.dialect, input.model)
		var missing *prompts.LookupError
		require.ErrorAs(t, err, &missing)
		assert.Equal(t, input.dialect, missing.Dialect)
		assert.Equal(t, input.model, missing.Model)
	}
}
