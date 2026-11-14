package core_test

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/config/configtest"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/core"
	"github.com/gfx-labs/towa/src/tools/jobs"
	"github.com/gfx-labs/towa/src/tools/toolset"
	"github.com/gfx-labs/towa/src/transcript"
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
	loaded, err := configtest.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config.Tools
	cfg.Todo.Enabled = true
	env := tools.Env{Cwd: t.TempDir()}
	var tracker core.ReadTracker
	var shown []acp.PlanEntry
	set := assemble(t, env, cfg, core.Deps{Reads: &tracker, Jobs: jobs.NewSet(), ShowPlan: func(_ context.Context, _ tools.Env, entries []acp.PlanEntry) error {
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
	replace := func(old, replacement string) map[string]any {
		return map[string]any{"file_path": path, "edits": []map[string]any{{"old_string": old, "new_string": replacement}}}
	}
	result = run(t, set, env, "edit", replace("second", "SECOND"))
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "first line\nSECOND line\n", string(actual))

	// Another session has read nothing, so it may not overwrite the file.
	var other core.ReadTracker
	foreign := assemble(t, env, cfg, core.Deps{Reads: &other, Jobs: jobs.NewSet()})
	result = run(t, foreign, env, "write", write)
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)

	// A change from outside is refused until the file is read again.
	require.NoError(t, os.WriteFile(path, []byte("first line\nEXTERN line\n"), 0o600))
	result = run(t, set, env, "edit", replace("second", "SECOND"))
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
	result = run(t, set, env, "read", map[string]any{"file_path": path, "limit": 1})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.Equal(t, "first line\n[More lines remain. Continue with offset 2.]", result.Parts[0].Text)
	result = run(t, set, env, "edit", replace("EXTERN", "reread"))
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)

	// An edit whose text is not in the file changes nothing, so the read
	// still stands for the corrected edit that follows.
	result = run(t, set, env, "edit", replace("absent", "reread"))
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
	result = run(t, set, env, "edit", replace("reread", "corrected"))
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome, result.Parts[0].Text)

	// One call makes several replacements, each on the result of the ones
	// before it: the second finds text the first wrote.
	several := func(edits ...map[string]any) map[string]any {
		return map[string]any{"file_path": path, "edits": edits}
	}
	result = run(t, set, env, "edit", several(
		map[string]any{"old_string": "first", "new_string": "line line"},
		map[string]any{"old_string": "corrected", "new_string": "last"},
		map[string]any{"old_string": "line line", "new_string": "opening"},
		map[string]any{"old_string": "line", "new_string": "row", "replace_all": true},
	))
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome, result.Parts[0].Text)
	require.Len(t, result.Content, 1)
	actual, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "opening row\nlast row\n", string(actual))

	// When one edit of a call fails the file is left as it was, the result
	// names that edit, and the session may still edit the file.
	for want, edits := range map[string][]map[string]any{
		"edits[1].old_string was not found":   {{"old_string": "opening", "new_string": "x"}, {"old_string": "opening", "new_string": "y"}},
		"edits[1].old_string occurs 2 times":  {{"old_string": "last", "new_string": "final"}, {"old_string": "row", "new_string": "line"}},
		"edits[0]: old_string and new_string": {{"old_string": "row", "new_string": "row"}},
		"edits[0].old_string is empty":        {{"old_string": "", "new_string": "x"}},
	} {
		result = run(t, set, env, "edit", several(edits...))
		require.Equal(t, transcript.OutcomeFailed, result.Outcome, want)
		require.Contains(t, result.Parts[0].Text, want)
		actual, err = os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "opening row\nlast row\n", string(actual), want)
	}
	result = run(t, set, env, "edit", replace("opening", "first"))
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome, result.Parts[0].Text)

	// edit changes a file that exists. A new file is write's to make.
	missing := filepath.Join(env.Cwd, "missing.txt")
	result = run(t, set, env, "edit", map[string]any{"file_path": missing, "edits": []map[string]any{{"old_string": "a", "new_string": "b"}}})
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "Use write")
	require.NoFileExists(t, missing)

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
	loaded, err := configtest.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config.Tools
	env := tools.Env{Cwd: t.TempDir()}
	set := assemble(t, env, cfg, core.Deps{Reads: new(core.ReadTracker), Jobs: jobs.NewSet()})
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
	result = run(t, set, env, "edit", map[string]any{"file_path": existing, "edits": []map[string]any{{"old_string": "new", "new_string": "newer"}}})
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

	// A later hunk moves the file an earlier hunk wrote. The patch is
	// applied, and the file that is left can be edited.
	text = "*** Begin Patch\n*** Update File: created.txt\n*** Move to: moved.txt\n@@\n-hello\n+moved\n*** Update File: moved.txt\n*** Move to: final.txt\n@@\n-moved\n+final\n*** End Patch"
	result = run(t, set, env, "apply_patch", map[string]any{"patch_text": text})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome, result.Parts[0].Text)
	require.NoFileExists(t, filepath.Join(env.Cwd, "moved.txt"))
	final := filepath.Join(env.Cwd, "final.txt")
	result = run(t, set, env, "edit", map[string]any{"file_path": final, "edits": []map[string]any{{"old_string": "final", "new_string": "edited"}}})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome, result.Parts[0].Text)

	result = run(t, set, env, "apply_patch", map[string]any{"patch_text": "not a patch"})
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
}

func TestBashRunsACommandAndRefusesABadTimeout(t *testing.T) {
	loaded, err := configtest.Load(config.Sources{})
	require.NoError(t, err)
	env := tools.Env{Cwd: t.TempDir()}
	set := assemble(t, env, loaded.Config.Tools, core.Deps{Reads: new(core.ReadTracker), Jobs: jobs.NewSet()})
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

func TestBackgroundCommandsWriteUnderTMPDIRAndAreWaitedForAndStopped(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	loaded, err := configtest.Load(config.Sources{})
	require.NoError(t, err)
	env := tools.Env{SessionID: "s1", Cwd: t.TempDir()}
	jobSet := jobs.NewSet()
	set := assemble(t, env, loaded.Config.Tools, core.Deps{Reads: new(core.ReadTracker), Jobs: jobSet})

	result := run(t, set, env, "bash", map[string]any{"command": "echo started; sleep 0.2; echo done; exit 3", "run_in_background": true})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	paths := regexp.MustCompile(`(/\S+\.out)`).FindStringSubmatch(result.Parts[0].Text)
	require.Len(t, paths, 2, result.Parts[0].Text)
	require.True(t, strings.HasPrefix(paths[1], tmp+string(filepath.Separator)), paths[1])

	result = run(t, set, env, "bash_output", map[string]any{"job_id": "bg1", "block": true})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "exited with code 3")
	require.Contains(t, result.Parts[0].Text, "started\ndone")
	data, err := os.ReadFile(paths[1])
	require.NoError(t, err)
	require.Equal(t, "started\ndone\n", string(data))
	require.Empty(t, jobSet.Take(), "a job the model waited for is not reported again")

	result = run(t, set, env, "bash", map[string]any{"command": "echo waiting; sleep 30", "run_in_background": true})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	result = run(t, set, env, "bash_output", map[string]any{"job_id": "bg2"})
	require.Contains(t, result.Parts[0].Text, "still running")
	result = run(t, set, env, "bash_stop", map[string]any{"job_id": "bg2"})
	require.Equal(t, transcript.OutcomeCompleted, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "was stopped")

	result = run(t, set, env, "bash_output", map[string]any{"job_id": "bg9"})
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "bg1, bg2")

	require.NoError(t, jobSet.Close())
	require.NoDirExists(t, filepath.Dir(paths[1]))
	result = run(t, set, env, "bash", map[string]any{"command": "true", "run_in_background": true})
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
}
