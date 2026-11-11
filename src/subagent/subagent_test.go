package subagent_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/harness"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/loop/looptest"
	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/subagent"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/convsearch"
	"github.com/stretchr/testify/require"
)

// The real child loop must store ownership, ask the parent's permission,
// return its answer and leave that answer searchable from the parent.
func TestChildTurn(t *testing.T) {
	f := looptest.Replay(t, looptest.ReadTurn)
	ctx := t.Context()
	id := uuid.NewV7()
	now := time.Now()
	require.NoError(t, f.Store.Create(ctx, store.Meta{ID: id, Cwd: f.Cwd, Created: now, Updated: now}))
	parent := f.Turn(acp.SessionId(id.String()), func(context.Context, acp.SessionUpdate) error { return nil })
	s, err := subagent.New(subagent.Options{Config: config.Subagents{MaxDepth: 1, MaxConcurrent: 1}})
	require.NoError(t, err)
	permits := 0
	parent.Permit = func(childCtx context.Context, _ string, _ tools.Effect, _ acp.ToolCallUpdate) (bool, error) {
		permits++
		_, nestedErr := s.Run(childCtx, subagent.Parent{Turn: parent, Loop: f.Loop}, subagent.Input{Prompt: "nested"})
		var nested *subagent.Error
		require.ErrorAs(t, nestedErr, &nested)
		require.Equal(t, subagent.KindTooDeep, nested.Kind)
		_, concurrentErr := s.Run(ctx, subagent.Parent{Turn: parent, Loop: f.Loop}, subagent.Input{Prompt: "concurrent"})
		var concurrent *subagent.Error
		require.ErrorAs(t, concurrentErr, &concurrent)
		require.Equal(t, subagent.KindTooMany, concurrent.Kind)
		return true, nil
	}
	_, err = s.Run(ctx, subagent.Parent{Turn: parent, Loop: f.Loop}, subagent.Input{Prompt: looptest.ReadTurn.Prompt, Model: "not-allowed"})
	var refused *subagent.Error
	require.ErrorAs(t, err, &refused)
	require.Equal(t, subagent.KindModelNotAllowed, refused.Kind)
	require.Zero(t, f.Served())
	var progress []acp.ToolCallContent
	result, err := s.Run(ctx, subagent.Parent{Turn: parent, Loop: f.Loop, Progress: func(_ context.Context, content []acp.ToolCallContent) error {
		progress = append(progress, content...)
		return nil
	}}, subagent.Input{Prompt: looptest.ReadTurn.Prompt, Description: "Read the launch code"})
	require.NoError(t, err)
	require.Contains(t, result.Final, "7421")
	require.Positive(t, permits)
	require.NotEmpty(t, progress)
	child, err := uuid.Parse(string(result.SessionID))
	require.NoError(t, err)
	meta, _, err := f.Store.Load(ctx, child)
	require.NoError(t, err)
	require.Equal(t, id, meta.ParentID)
	hits, err := convsearch.Search(ctx, f.Store, convsearch.Access{Session: id, Widest: convsearch.ScopeDescendants}, convsearch.Limits{Hits: 100, SnippetContextBytes: 1024, MessageBytes: 1024, ToolResultBytes: 1024}, convsearch.SearchInput{Target: convsearch.Target{Scope: convsearch.ScopeDescendants}, Query: "7421"})
	require.NoError(t, err)
	require.NotEmpty(t, hits.Hits)
	for _, hit := range hits.Hits {
		require.Equal(t, child, hit.Session)
	}
}

func TestChildLimitIsAnError(t *testing.T) {
	scenario := looptest.ReadTurn
	scenario.Requests = 1
	f := looptest.Replay(t, scenario)
	parent := f.Turn(acp.SessionId(uuid.NewV7().String()), func(context.Context, acp.SessionUpdate) error { return nil })
	parent.Limits = &config.Limits{MaxToolCalls: 1, MaxOutputTokens: 1}
	s, err := subagent.New(subagent.Options{Config: config.Subagents{MaxDepth: 1}})
	require.NoError(t, err)
	_, err = s.Run(t.Context(), subagent.Parent{Turn: parent, Loop: f.Loop}, subagent.Input{Prompt: looptest.ReadTurn.Prompt})
	var failed *subagent.Error
	require.ErrorAs(t, err, &failed)
	require.Equal(t, subagent.KindStopped, failed.Kind)
	require.Equal(t, harness.StopLimit, failed.Stop.Kind)
	require.NotEmpty(t, failed.SessionID)
}
