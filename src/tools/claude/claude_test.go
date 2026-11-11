package claude_test

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/claude"
	"github.com/gfx-labs/acpharness/src/tools/generic"
	"github.com/gfx-labs/acpharness/src/toolset"
	"github.com/gfx-labs/acpharness/src/transcript"
	"github.com/stretchr/testify/require"
)

func runAdapter(t *testing.T, set toolset.Result, env tools.Env, name string, input any) tools.Result {
	t.Helper()
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	checked, _, err := set.Schemas[name].Apply(raw, false)
	require.NoError(t, err)
	tool, ok := set.Set.Lookup(name)
	require.True(t, ok)
	result, err := tool.Run(t.Context(), tools.Call{Input: checked, Env: env})
	require.NoError(t, err)
	return result
}

func TestCapturedCoreAdaptersShareSessionStateAcrossDialects(t *testing.T) {
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config.Tools
	env := tools.Env{Cwd: t.TempDir()}
	var tracker claude.ReadTracker
	var shown []acp.PlanEntry
	deps := claude.Deps{Reads: &tracker, ShowPlan: func(_ context.Context, _ tools.Env, entries []acp.PlanEntry) error {
		shown = entries
		return nil
	}}
	neutral, err := toolset.Assemble(t.Context(), toolset.Input{Dialect: generic.Dialect, Gate: cfg.Gate, Tools: func() ([]tools.Tool, error) {
		return generic.Tools(env, cfg, deps)
	}})
	require.NoError(t, err)
	captured, err := toolset.Assemble(t.Context(), toolset.Input{Dialect: claude.Dialect, Gate: cfg.Gate, Tools: func() ([]tools.Tool, error) {
		return claude.Tools(env, cfg, deps)
	}})
	require.NoError(t, err)

	path := filepath.Join(env.Cwd, "session.txt")
	write := map[string]any{"file_path": path, "content": "first line\nsecond line\n"}
	result := runAdapter(t, neutral, env, "Write", write)
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.Len(t, result.Content, 1)
	require.Equal(t, path, result.Content[0].Diff.Path)
	edit := map[string]any{"file_path": path, "old_string": "second", "new_string": "SECOND"}
	result = runAdapter(t, captured, env, "Edit", edit)
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "first line\nSECOND line\n", string(actual))

	// A newly constructed dialect retains authority only through the session's tracker.
	var other claude.ReadTracker
	foreign, err := toolset.Assemble(t.Context(), toolset.Input{Dialect: generic.Dialect, Tools: func() ([]tools.Tool, error) {
		return generic.Tools(env, cfg, generic.Deps{Reads: &other})
	}})
	require.NoError(t, err)
	result = runAdapter(t, foreign, env, "Write", write)
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
	require.NoError(t, os.WriteFile(path, []byte("first line\nEXTERN line\n"), 0o600))
	result = runAdapter(t, neutral, env, "Edit", edit)
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
	result = runAdapter(t, captured, env, "Read", map[string]any{"file_path": path, "limit": 1})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "1\tfirst line")
	edit["old_string"], edit["new_string"] = "EXTERN", "reread"
	result = runAdapter(t, captured, env, "Edit", edit)
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)

	result = runAdapter(t, neutral, env, "TodoWrite", map[string]any{"todos": []map[string]any{{"content": "Check files", "status": "in_progress", "activeForm": "Checking files"}}})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.Len(t, shown, 1)
	require.Equal(t, "Checking files", shown[0].Content)
	require.Equal(t, acp.PlanEntryStatusInProgress, shown[0].Status)

	imagePath := filepath.Join(env.Cwd, "picture.png")
	imageData := fixture(t, "blue-purple-pink-large.png")
	require.NoError(t, os.WriteFile(imagePath, imageData, 0o600))
	result = runAdapter(t, captured, env, "Read", map[string]any{"file_path": imagePath})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.NotNil(t, result.Parts[0].Image)
	require.Equal(t, imageData, result.Parts[0].Image.Data)
	result = runAdapter(t, captured, env, "Read", map[string]any{"file_path": path, "pages": "1"})
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
}

func TestCapturedBashRunsAndRefusesUnsupportedModes(t *testing.T) {
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	env := tools.Env{Cwd: t.TempDir()}
	set, err := toolset.Assemble(t.Context(), toolset.Input{Dialect: generic.Dialect, Tools: func() ([]tools.Tool, error) {
		return generic.Tools(env, loaded.Config.Tools, generic.Deps{Reads: new(claude.ReadTracker)})
	}})
	require.NoError(t, err)
	result := runAdapter(t, set, env, "Bash", map[string]any{"command": "printf 'real command output'; printf saved > created.txt", "timeout": 2000.5})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "real command output")
	data, err := os.ReadFile(filepath.Join(env.Cwd, "created.txt"))
	require.NoError(t, err)
	require.Equal(t, "saved", string(data))
	result = runAdapter(t, set, env, "Bash", map[string]any{"command": "printf failed; exit 7"})
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "Exit code 7")
	for _, option := range []map[string]any{
		{"run_in_background": true},
		{"dangerouslyDisableSandbox": true},
		{"timeout": -1},
		{"timeout": 0.0000001},
	} {
		option["command"] = "printf forbidden > refused.txt"
		result = runAdapter(t, set, env, "Bash", option)
		require.Equal(t, transcript.OutcomeFailed, result.Outcome)
		_, err = os.Stat(filepath.Join(env.Cwd, "refused.txt"))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	result = runAdapter(t, set, env, "Bash", map[string]any{"command": "sleep 30", "timeout": 20})
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "timed out")
}

func TestCapturedSearchAdaptersRunRealRipgrep(t *testing.T) {
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	env := tools.Env{Cwd: t.TempDir()}
	path := filepath.Join(env.Cwd, "found.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha\nneedle\nneedle\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(env.Cwd, "other.go"), []byte("alpha\n"), 0o600))
	set, err := toolset.Assemble(t.Context(), toolset.Input{Dialect: generic.Dialect, Tools: func() ([]tools.Tool, error) {
		return generic.Tools(env, loaded.Config.Tools, generic.Deps{Reads: new(claude.ReadTracker)})
	}})
	require.NoError(t, err)
	result := runAdapter(t, set, env, "Glob", map[string]any{"pattern": "*.txt"})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.Equal(t, path, result.Parts[0].Text)
	for _, mode := range []string{"files_with_matches", "content", "count"} {
		t.Run(mode, func(t *testing.T) {
			result := runAdapter(t, set, env, "Grep", map[string]any{"pattern": "needle", "output_mode": mode, "head_limit": 0})
			require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
			require.Contains(t, result.Parts[0].Text, path)
			if mode == "content" {
				require.Contains(t, result.Parts[0].Text, ":2:needle")
			}
			if mode == "count" {
				require.Contains(t, result.Parts[0].Text, ":2")
			}
		})
	}
	result = runAdapter(t, set, env, "Grep", map[string]any{"pattern": "needle", "output_mode": "content", "-o": true})
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
}
