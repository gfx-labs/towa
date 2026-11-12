package acp_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/pkg/acp"
)

// session/new must answer before a notification names its new session. The
// handler queues that notification before returning its result.
func TestAfterResponseOrdersNotifications(t *testing.T) {
	for _, inOrder := range []bool{false, true} {
		t.Run(map[bool]string{false: "prompt", true: "initialize"}[inOrder], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ag := &agent{}
				conn, wire := serveAgent(t, ag)
				var handlerCtx context.Context
				queue := func(ctx context.Context) {
					handlerCtx = ctx
					for _, text := range []string{"first", "second"} {
						require.True(t, acp.AfterResponse(ctx, func() {
							require.False(t, acp.AfterResponse(ctx, func() {}))
							require.NoError(t, conn.SessionUpdate(ctx, agentMessage("s", text)))
						}))
					}
				}
				if inOrder {
					ag.initialize = func(ctx context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
						queue(ctx)
						return acp.InitializeResponse{ProtocolVersion: 1}, nil
					}
					wire.Send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
				} else {
					ag.prompt = func(ctx context.Context, _ acp.PromptRequest) (acp.PromptResponse, error) {
						queue(ctx)
						return acp.PromptResponse{}, acp.NewAuthRequired(nil)
					}
					wire.Send(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`)
				}
				response := wire.Recv()
				require.Equal(t, "1", string(response.ID))
				for _, text := range []string{"first", "second"} {
					n := wire.Recv()
					require.Equal(t, "session/update", n.Method)
					require.Contains(t, string(n.Params), text)
				}
				require.False(t, acp.AfterResponse(handlerCtx, func() {}))
				require.False(t, acp.AfterResponse(t.Context(), func() {}))
			})
		})
	}
}
