package codex

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/repair"
	"github.com/gfx-labs/acpharness/src/transcript"
	"github.com/stretchr/testify/require"
)

// Ported from utils/string/src/truncate/tests.rs:89-124 at ce0323f2.
func TestUpstreamTokenTruncation(t *testing.T) {
	for _, tc := range []struct {
		input  string
		tokens int64
		want   string
	}{
		{"short output", 100, "short output"},
		{"abcdef", 0, "…2 tokens truncated…"},
		{"😀😀😀😀😀😀😀😀😀😀\nsecond line with text\n", 8, "😀😀😀😀…8 tokens truncated… line with text\n"},
		{"a😀b😀c", 1, "a…2 tokens truncated…c"},
		{"é中😀é", 1, "é…2 tokens truncated…é"},
		{"😀😀😀", 1, "…2 tokens truncated…"},
	} {
		require.Equal(t, tc.want, truncate(tc.input, tc.tokens))
	}
}

// Composed integration inputs, using real shell commands and temporary files.
func TestDialectRunsCommandsAndReportsPartialPatch(t *testing.T) {
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	env := tools.Env{Cwd: t.TempDir()}
	dialect, err := Tools(env, loaded.Config.Tools, Deps{})
	require.NoError(t, err)
	set := tools.Set{Tools: dialect}
	command, ok := set.Lookup("exec_command")
	require.True(t, ok)
	schema, err := repair.Compile(command.Spec.InputSchema)
	require.NoError(t, err)
	for _, tc := range []struct {
		input    string
		outcome  transcript.Outcome
		contains string
	}{
		{`{"cmd":"printf hello","login":false}`, transcript.OutcomeCompleted, "Output:\nhello"},
		{`{"cmd":"printf problem; exit 7","login":false}`, transcript.OutcomeFailed, "Process exited with code 7"},
		{`{"cmd":"sleep 1","timeout_ms":10,"login":false}`, transcript.OutcomeFailed, "Process exited with code 124"},
		{`{"cmd":"shopt -q login_shell","shell":"/bin/bash"}`, transcript.OutcomeCompleted, "Process exited with code 0"},
	} {
		input, _, err := schema.Apply([]byte(tc.input), false)
		require.NoError(t, err)
		result, err := command.Run(context.Background(), tools.Call{Input: input, Env: env})
		require.NoError(t, err)
		require.Equal(t, tc.outcome, result.Outcome)
		require.Contains(t, result.Parts[0].Text, tc.contains)
	}
	tool, ok := set.Lookup("apply_patch")
	require.True(t, ok)
	text := "*** Begin Patch\n*** Add File: created.txt\n+hello\n*** Update File: missing.txt\n@@\n-old\n+new\n*** End Patch"
	input, err := json.Marshal(text)
	require.NoError(t, err)
	desc, err := tool.Describe(input)
	require.NoError(t, err)
	require.Contains(t, desc.WritePaths, filepath.Join(env.Cwd, "created.txt"))
	result, err := tool.Run(context.Background(), tools.Call{Input: input, Env: env})
	require.NoError(t, err)
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
	require.Contains(t, result.Parts[0].Text, "Changes applied before the failure:\nA created.txt\n")
	require.Len(t, result.Content, 2)
	got, err := os.ReadFile(filepath.Join(env.Cwd, "created.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello\n", string(got))
}

// Composed plan input checks delivery through the production callback boundary.
func TestPlanDelivery(t *testing.T) {
	var got []acp.PlanEntry
	env := tools.Env{Cwd: t.TempDir()}
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	list, err := Tools(env, loaded.Config.Tools, Deps{ShowPlan: func(_ context.Context, _ tools.Env, entries []acp.PlanEntry) error { got = entries; return nil }})
	require.NoError(t, err)
	tool, ok := (tools.Set{Tools: list}).Lookup("update_plan")
	require.True(t, ok)
	schema, err := repair.Compile(tool.Spec.InputSchema)
	require.NoError(t, err)
	input, _, err := schema.Apply([]byte(`{"plan":[{"step":"Read the source","status":"completed"},{"step":"Run checks","status":"in_progress"}]}`), false)
	require.NoError(t, err)
	result, err := tool.Run(context.Background(), tools.Call{Input: input, Env: env})
	require.NoError(t, err)
	require.Equal(t, "Plan updated", result.Parts[0].Text)
	require.Len(t, got, 2)
	require.Equal(t, "Run checks", got[1].Content)
	require.Equal(t, acp.PlanEntryStatusInProgress, got[1].Status)
}
