package core_test

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/core"
	"github.com/gfx-labs/acpharness/src/toolset"
	"github.com/gfx-labs/acpharness/src/transcript"
	"github.com/stretchr/testify/require"
)

// assemble builds the core tools as a session does, with the gate's member
// added to every schema.
func assemble(t *testing.T, env tools.Env, cfg config.Tools, deps core.Deps) toolset.Result {
	t.Helper()
	set, err := toolset.Assemble(t.Context(), toolset.Input{Gate: cfg.Gate, Tools: func() ([]tools.Tool, error) {
		return core.Tools(env, cfg, deps)
	}})
	require.NoError(t, err)
	return set
}

// run validates the input against the tool's schema, as the loop does, and
// runs the tool.
func run(t *testing.T, set toolset.Result, env tools.Env, name string, input any) tools.Result {
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

func TestFileToolsShareOneSessionsReadState(t *testing.T) {
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config.Tools
	env := tools.Env{Cwd: t.TempDir()}
	var tracker core.ReadTracker
	var shown []acp.PlanEntry
	set := assemble(t, env, cfg, core.Deps{Reads: &tracker, ShowPlan: func(_ context.Context, _ tools.Env, entries []acp.PlanEntry) error {
		shown = entries
		return nil
	}})

	path := filepath.Join(env.Cwd, "session.txt")
	write := map[string]any{"file_path": path, "content": "first line\nsecond line\n"}
	result := run(t, set, env, "write", write)
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.Len(t, result.Content, 1)
	require.Equal(t, path, result.Content[0].Diff.Path)
	// The session wrote the file, so it may edit it without a read.
	edit := map[string]any{"file_path": path, "old_string": "second", "new_string": "SECOND"}
	result = run(t, set, env, "edit", edit)
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "first line\nSECOND line\n", string(actual))

	// Another session has read nothing, so it may not overwrite the file.
	var other core.ReadTracker
	foreign := assemble(t, env, cfg, core.Deps{Reads: &other})
	result = run(t, foreign, env, "write", write)
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)

	// A change from outside is refused until the file is read again.
	require.NoError(t, os.WriteFile(path, []byte("first line\nEXTERN line\n"), 0o600))
	result = run(t, set, env, "edit", edit)
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
	result = run(t, set, env, "read", map[string]any{"file_path": path, "limit": 1})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "1\tfirst line")
	edit["old_string"], edit["new_string"] = "EXTERN", "reread"
	result = run(t, set, env, "edit", edit)
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)

	result = run(t, set, env, "todo", map[string]any{
		"todos": []map[string]any{{"content": "Check files", "status": "in_progress", "activeForm": "Checking files", "confidence": "plausible"}},
		"plan":  map[string]any{"user_intention": "Check the files", "understands_user_intent": "clear"},
		"goals": []map[string]any{{
			"closed_feedback_loop": "usable", "feedback_loop": "read each file back", "feedback_loop_relevance": "representative",
			"feedback_loop_coverage": "main_paths", "feedback_loop_traceability": "partial",
		}},
	})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.Len(t, shown, 1)
	require.Equal(t, "Checking files", shown[0].Content)
	require.Equal(t, acp.PlanEntryStatusInProgress, shown[0].Status)

	imagePath := filepath.Join(env.Cwd, "picture.png")
	imageData := fixture(t, "blue-purple-pink-large.png")
	require.NoError(t, os.WriteFile(imagePath, imageData, 0o600))
	result = run(t, set, env, "read", map[string]any{"file_path": imagePath})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.NotNil(t, result.Parts[0].Image)
	require.Equal(t, imageData, result.Parts[0].Image.Data)
}

func TestApplyPatchChangesFilesAndKeepsWhatWasAppliedBeforeAFailure(t *testing.T) {
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config.Tools
	env := tools.Env{Cwd: t.TempDir()}
	set := assemble(t, env, cfg, core.Deps{Reads: new(core.ReadTracker)})
	existing := filepath.Join(env.Cwd, "existing.txt")
	require.NoError(t, os.WriteFile(existing, []byte("keep\nold\n"), 0o600))
	doomed := filepath.Join(env.Cwd, "doomed.txt")
	require.NoError(t, os.WriteFile(doomed, []byte("gone\n"), 0o600))

	// A file the session never read is updated: the hunk's lines are the check.
	text := "*** Begin Patch\n*** Add File: created.txt\n+hello\n*** Update File: existing.txt\n@@\n keep\n-old\n+new\n*** Delete File: doomed.txt\n*** End Patch"
	input, err := json.Marshal(map[string]any{"patch_text": text})
	require.NoError(t, err)
	tool, ok := set.Set.Lookup("apply_patch")
	require.True(t, ok)
	desc, err := tool.Describe(input)
	require.NoError(t, err)
	created := filepath.Join(env.Cwd, "created.txt")
	require.ElementsMatch(t, []string{created, existing, doomed}, desc.WritePaths)

	result := run(t, set, env, "apply_patch", map[string]any{"patch_text": text})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome, result.Parts[0].Text)
	require.Len(t, result.Content, 3)
	got, err := os.ReadFile(existing)
	require.NoError(t, err)
	require.Equal(t, "keep\nnew\n", string(got))
	got, err = os.ReadFile(created)
	require.NoError(t, err)
	require.Equal(t, "hello\n", string(got))
	require.NoFileExists(t, doomed)

	// The session wrote both files, so edit follows without a read.
	result = run(t, set, env, "edit", map[string]any{"file_path": existing, "old_string": "new", "new_string": "newer"})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome, result.Parts[0].Text)

	// The second file's hunk does not match. The first file stays written
	// and the result says so.
	text = "*** Begin Patch\n*** Add File: partial.txt\n+kept\n*** Update File: existing.txt\n@@\n-absent line\n+x\n*** End Patch"
	result = run(t, set, env, "apply_patch", map[string]any{"patch_text": text})
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "Changes applied before the failure:\nA partial.txt\n")
	got, err = os.ReadFile(filepath.Join(env.Cwd, "partial.txt"))
	require.NoError(t, err)
	require.Equal(t, "kept\n", string(got))

	result = run(t, set, env, "apply_patch", map[string]any{"patch_text": "not a patch"})
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
}

func TestBashRunsACommandAndRefusesABadTimeout(t *testing.T) {
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	env := tools.Env{Cwd: t.TempDir()}
	set := assemble(t, env, loaded.Config.Tools, core.Deps{Reads: new(core.ReadTracker)})
	result := run(t, set, env, "bash", map[string]any{"command": "printf 'real command output'; printf saved > created.txt", "timeout": 2000.5})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "real command output")
	data, err := os.ReadFile(filepath.Join(env.Cwd, "created.txt"))
	require.NoError(t, err)
	require.Equal(t, "saved", string(data))
	result = run(t, set, env, "bash", map[string]any{"command": "printf failed; exit 7"})
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "Exit code 7")
	for _, timeout := range []float64{-1, 0.0000001} {
		result = run(t, set, env, "bash", map[string]any{"command": "printf forbidden > refused.txt", "timeout": timeout})
		require.Equal(t, transcript.OutcomeFailed, result.Outcome)
		require.NoFileExists(t, filepath.Join(env.Cwd, "refused.txt"))
	}
	result = run(t, set, env, "bash", map[string]any{"command": "sleep 30", "timeout": 20})
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "timed out")
}

func TestGlobAndGrepRunRealRipgrep(t *testing.T) {
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	env := tools.Env{Cwd: t.TempDir()}
	path := filepath.Join(env.Cwd, "found.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha\nneedle\nneedle\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(env.Cwd, "other.go"), []byte("alpha\n"), 0o600))
	set := assemble(t, env, loaded.Config.Tools, core.Deps{Reads: new(core.ReadTracker)})
	result := run(t, set, env, "glob", map[string]any{"pattern": "*.txt"})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.Equal(t, path, result.Parts[0].Text)
	for _, mode := range []string{"files_with_matches", "content", "count"} {
		t.Run(mode, func(t *testing.T) {
			result := run(t, set, env, "grep", map[string]any{"pattern": "needle", "output_mode": mode, "head_limit": 0})
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
}
