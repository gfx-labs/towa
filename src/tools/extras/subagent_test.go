package extras_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"testing"
	"uuid"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/loop"
	"github.com/gfx-labs/towa/src/loop/looptest"
	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/subagent"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/extras"
	"github.com/gfx-labs/towa/src/transcript"
	"github.com/stretchr/testify/require"
)

// Invoke the wrapper inside a real dispatched call's permission context.
// Parent and child use distinct real loops and recorded clients.
func TestSubagentToolChargesSuccessfulAndFailedChildUsage(t *testing.T) {
	for _, failChild := range []bool{false, true} {
		name := "completed"
		if failChild {
			name = "permission_failure_after_usage"
		}
		t.Run(name, func(t *testing.T) {
			parentFixture := looptest.Replay(t, looptest.ReadTurn)
			childScenario := looptest.ReadTurn
			if failChild {
				childScenario.Requests = 1
			}
			childFixture := looptest.Replay(t, childScenario)
			parentID := acp.SessionId(uuid.NewV7().String())
			options := parentFixture.Options
			parentResolve := options.Resolve
			options.Resolve = func(ctx context.Context, name string) (loop.Resolved, error) {
				require.Equal(t, "current-selection", name)
				return parentResolve(ctx, "")
			}
			parentLoop, err := loop.New(options)
			require.NoError(t, err)
			spawner, err := subagent.New(subagent.Options{Config: config.Subagents{MaxDepth: 1, MaxConcurrent: 1}})
			require.NoError(t, err)
			var childLoop *loop.Loop
			childRunner := childRuntime(func(ctx context.Context, turn loop.Turn) (loop.Result, error) {
				require.Equal(t, parentID, turn.ParentID)
				require.NotEqual(t, parentID, turn.SessionID)
				childOptions := childFixture.Options
				childOptions.Store = parentFixture.Store
				resolve := childOptions.Resolve
				childOptions.Resolve = func(ctx context.Context, name string) (loop.Resolved, error) {
					require.Equal(t, "current-selection", name)
					return resolve(ctx, "")
				}
				var openErr error
				childLoop, openErr = loop.New(childOptions)
				if openErr != nil {
					return loop.Result{}, openErr
				}
				return childLoop.Run(ctx, turn)
			})
			tool, err := extras.Subagent(spawner, childRunner)
			require.NoError(t, err)
			input, err := json.Marshal(map[string]string{"prompt": looptest.ReadTurn.Prompt, "description": "Read launch code"})
			require.NoError(t, err)
			var output struct {
				Final     string        `json:"final"`
				SessionID acp.SessionId `json:"session_id"`
				Usage     *acp.Usage    `json:"usage"`
				Charged   bool          `json:"charged"`
				Error     *struct {
					Kind subagent.Kind `json:"kind"`
				} `json:"error"`
			}
			var outcome transcript.Outcome
			invoked := false
			childPermissionError := errors.New("child permission transport failed")
			parentTurn := parentFixture.Turn(parentID, func(context.Context, acp.SessionUpdate) error { return nil })
			parentTurn.Model = "current-selection"
			parentTurn.Permit = func(ctx context.Context, _ string, _ tools.Effect, _ acp.ToolCallUpdate) (bool, error) {
				current, ok := loop.Current(ctx)
				require.True(t, ok)
				if current.Turn.SessionID != parentID {
					require.Same(t, childLoop, current.Loop)
					if failChild {
						return false, childPermissionError
					}
					return true, nil
				}
				require.Same(t, parentLoop, current.Loop)
				require.False(t, invoked)
				invoked = true
				result, err := tool.Run(ctx, tools.Call{ID: "child", Input: jsontext.Value(input), Env: tools.Env{SessionID: parentID, Cwd: current.Turn.Cwd}})
				require.NoError(t, err)
				require.Len(t, result.Parts, 1)
				require.NoError(t, json.Unmarshal([]byte(result.Parts[0].Text), &output))
				outcome = result.Outcome
				return true, nil
			}
			result, err := parentLoop.Run(t.Context(), parentTurn)
			require.NoError(t, err)
			require.True(t, invoked)
			if failChild {
				require.Equal(t, transcript.OutcomeFailed, outcome)
				require.NotNil(t, output.Error)
				require.Equal(t, subagent.KindFailed, output.Error.Kind)
				require.Empty(t, output.Final)
			} else {
				require.Equal(t, transcript.OutcomeCompleted, outcome)
				require.Nil(t, output.Error)
				require.Contains(t, output.Final, "7421")
			}
			childID, err := uuid.Parse(string(output.SessionID))
			require.NoError(t, err)
			parentUUID, err := uuid.Parse(string(parentID))
			require.NoError(t, err)
			var inputTokens, outputTokens, childInputTokens int64
			for _, id := range []uuid.UUID{parentUUID, childID} {
				entries, err := parentFixture.Store.History(t.Context(), id, store.Range{})
				require.NoError(t, err)
				for entry, err := range entries {
					require.NoError(t, err)
					if usage := entry.Item.Usage; usage != nil {
						inputTokens += usage.InputTokens
						outputTokens += usage.OutputTokens
						if id == childID {
							childInputTokens += usage.InputTokens
						}
					}
				}
			}
			require.Positive(t, childInputTokens)
			require.True(t, output.Charged)
			require.NotNil(t, output.Usage)
			require.Equal(t, int(childInputTokens), output.Usage.InputTokens)
			require.NotNil(t, result.Usage)
			require.Equal(t, int(inputTokens), result.Usage.InputTokens)
			require.Equal(t, int(outputTokens), result.Usage.OutputTokens)
		})
	}
}

type childRuntime func(context.Context, loop.Turn) (loop.Result, error)

func (run childRuntime) Run(ctx context.Context, turn loop.Turn) (loop.Result, error) {
	return run(ctx, turn)
}
