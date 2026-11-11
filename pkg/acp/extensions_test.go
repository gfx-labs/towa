package acp_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
)

type extensionPeer interface {
	RegisterExtensionRequest(string, acp.MethodHandler) error
	RegisterExtensionNotification(string, acp.MethodHandler) error
	SendExtensionRequest(context.Context, string, any) (jsontext.Value, error)
	SendExtensionNotification(context.Context, string, any) error
}

func TestExtensionMethods(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, c := connect(t, &agent{}, &client{})
		for _, peers := range [][2]extensionPeer{{a, c}, {c, a}} {
			receiver, sender := peers[0], peers[1]
			var notifications []int
			request := func(ctx context.Context, method string, params jsontext.Value) (any, *acp.RequestError) {
				require.Equal(t, "_example.org/buffers", method)
				var p struct {
					Value int `json:"value"`
				}
				if err := json.Unmarshal(params, &p); err != nil {
					return nil, acp.NewInvalidParams(nil)
				}
				if p.Value == 0 {
					return nil, acp.NewResourceNotFound(map[string]any{"value": p.Value})
				}
				_, ok := acp.Arrival(ctx)
				require.True(t, ok)
				return map[string]any{"value": p.Value, "notifications": notifications}, nil
			}
			require.NoError(t, receiver.RegisterExtensionRequest("_example.org/buffers", request))
			require.NoError(t, receiver.RegisterExtensionNotification("_example.org/buffers", func(ctx context.Context, _ string, params jsontext.Value) (any, *acp.RequestError) {
				var p struct {
					Value int `json:"value"`
				}
				require.NoError(t, json.Unmarshal(params, &p))
				notifications = append(notifications, p.Value)
				_, err := receiver.SendExtensionRequest(ctx, "_other", nil)
				require.ErrorIs(t, err, acp.ErrWaitInOrder)
				require.False(t, acp.AfterResponse(ctx, func() {}))
				return nil, nil
			}))
			require.Error(t, receiver.RegisterExtensionRequest("session/prompt", request))
			require.Error(t, receiver.RegisterExtensionRequest("_nil", nil))
			require.Error(t, receiver.RegisterExtensionRequest("_example.org/buffers", request))
			require.Error(t, sender.SendExtensionNotification(t.Context(), "not_an_extension", nil))
			_, err := sender.SendExtensionRequest(t.Context(), "not_an_extension", nil)
			require.Error(t, err)

			// The notification of this name has a different handler from the
			// request. Both notifications finish before the later request.
			for _, value := range []int{1, 2} {
				require.NoError(t, sender.SendExtensionNotification(t.Context(), "_example.org/buffers", map[string]any{"value": value}))
			}
			require.NoError(t, sender.SendExtensionNotification(t.Context(), "_unknown", nil))
			result, err := sender.SendExtensionRequest(t.Context(), "_example.org/buffers", map[string]any{"value": 3})
			require.NoError(t, err)
			require.JSONEq(t, `{"value":3,"notifications":[1,2]}`, string(result))
			_, err = sender.SendExtensionRequest(t.Context(), "_unknown", nil)
			var requestError *acp.RequestError
			require.ErrorAs(t, err, &requestError)
			require.Equal(t, acp.NewMethodNotFound("").Code, requestError.Code)
			_, err = sender.SendExtensionRequest(t.Context(), "_example.org/buffers", map[string]any{"value": 0})
			require.ErrorAs(t, err, &requestError)
			require.Equal(t, acp.NewResourceNotFound(nil).Code, requestError.Code)
		}
	})
}
