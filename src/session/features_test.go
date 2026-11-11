package session_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/acptest"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/session"
	"github.com/gfx-labs/acpharness/src/tools"
)

func request(id, method, params string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":` + params + `}`
}

func endsTurn(context.Context, loop.Turn) (loop.Result, error) {
	return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
}

// create initializes with the given client capabilities and creates a
// session with the given extra members of session/new.
func create(t *testing.T, w *acptest.Wire, clientCapabilities, extra string) acp.NewSessionResponse {
	t.Helper()
	w.Send(
		request(`"init"`, "initialize", `{"protocolVersion":1,"clientCapabilities":`+clientCapabilities+`}`),
		request(`"new"`, "session/new", `{"cwd":"/work","mcpServers":[]`+extra+`}`),
	)
	got, _ := w.Await(`"init"`, `"new"`)
	require.Nil(t, got[`"new"`].Error)
	var resp acp.NewSessionResponse
	require.NoError(t, json.Unmarshal(got[`"new"`].Result, &resp))
	return resp
}

// An old client decodes an update type it does not know as another type,
// without an error. So the same turn reaches two clients differently.
func TestUpdatesBehindAClientCapabilityAreSentOnlyWhenAdvertised(t *testing.T) {
	notice := acp.SessionUpdate{Notice: &acp.SessionUpdateNotice{SessionUpdate: "notice", Severity: "info", Title: "fell back to another account"}}
	message := acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
		Content: acp.ContentBlock{Text: &acp.ContentBlockText{Type: "text", Text: "done"}},
	}}
	tests := []struct {
		name         string
		capabilities string
		wantNotice   bool
	}{
		{"a client that advertised nothing", `{}`, false},
		{"a client that advertised compaction only", `{"session":{"compaction":{}}}`, false},
		{"a client that advertised notices", `{"session":{"notices":{}},"terminal":true}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := serve(t, func(ctx context.Context, turn loop.Turn) (loop.Result, error) {
					// The turn can ask first, and is told what the client said.
					require.Equal(t, tt.wantNotice, turn.Sendable(notice))
					require.Equal(t, tt.wantNotice, turn.Client.Terminal)
					require.NoError(t, turn.Update(ctx, notice))
					require.NoError(t, turn.Update(ctx, message))
					return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
				})
				id := string(create(t, w, tt.capabilities, "").SessionId)

				w.Send(prompt(`1`, id, "go"))
				_, others := w.Await(`1`)
				updates := updatesOf(t, others)[1:] // after the commands
				if tt.wantNotice {
					require.Len(t, updates, 2)
					require.Equal(t, "fell back to another account", updates[0].Update.Notice.Title)
					updates = updates[1:]
				}
				require.Len(t, updates, 1)
				require.Equal(t, "done", updates[0].Update.AgentMessageChunk.Content.Text.Text)
			})
		})
	}
}

func TestModeIsSwitchedBySetModeAndBySetConfigOption(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := serve(t, endsTurn)
		created := create(t, w, `{}`, "")
		id := string(created.SessionId)

		require.Equal(t, acp.SessionModeId("auto"), created.Modes.CurrentModeId)
		require.Len(t, created.Modes.AvailableModes, 2)
		require.Len(t, created.ConfigOptions, 1)
		require.Equal(t, session.ModeAuto, created.ConfigOptions[0].Select.CurrentValue)

		// set_mode answers with nothing, so both views of the mode are sent.
		w.Send(request(`1`, "session/set_mode", `{"sessionId":"`+id+`","modeId":"ask"}`))
		got, others := w.Await(`1`)
		require.Nil(t, got[`1`].Error)
		updates := updatesOf(t, others)
		require.Len(t, updates, 3)
		require.NotNil(t, updates[0].Update.AvailableCommandsUpdate)
		require.Equal(t, acp.SessionModeId("ask"), updates[1].Update.CurrentModeUpdate.CurrentModeId)
		require.Equal(t, session.ModeAsk, updates[2].Update.ConfigOptionUpdate.ConfigOptions[0].Select.CurrentValue)

		// set_config_option answers with the options, so only the mode
		// update is sent.
		w.Send(request(`2`, "session/set_config_option", `{"sessionId":"`+id+`","configId":"mode","value":"auto"}`))
		got, others = w.Await(`2`)
		require.Nil(t, got[`2`].Error)
		var resp acp.SetSessionConfigOptionResponse
		require.NoError(t, json.Unmarshal(got[`2`].Result, &resp))
		require.Equal(t, session.ModeAuto, resp.ConfigOptions[0].Select.CurrentValue)
		updates = updatesOf(t, others)
		require.Len(t, updates, 1)
		require.Equal(t, acp.SessionModeId("auto"), updates[0].Update.CurrentModeUpdate.CurrentModeId)

		// What cannot be set is an error, and the mode stays.
		w.Send(
			request(`3`, "session/set_mode", `{"sessionId":"`+id+`","modeId":"yolo"}`),
			request(`4`, "session/set_config_option", `{"sessionId":"`+id+`","configId":"model","value":"opus"}`),
			request(`5`, "session/set_config_option", `{"sessionId":"`+id+`","configId":"mode","type":"boolean","value":true}`),
			request(`6`, "session/set_mode", `{"sessionId":"nope","modeId":"ask"}`),
		)
		got, others = w.Await(`3`, `4`, `5`, `6`)
		for _, n := range []string{`3`, `4`, `5`} {
			require.Equal(t, acp.NewInvalidParams(nil).Code, got[n].Error.Code, n)
		}
		require.Equal(t, acp.NewResourceNotFound(nil).Code, got[`6`].Error.Code)
		require.Empty(t, others)
	})
}

// permission is the client's side of one permission request.
type permission struct {
	id      string
	request acp.RequestPermissionRequest
}

// awaitPermission reads the agent's next message, which must be a
// permission request.
func awaitPermission(t *testing.T, w *acptest.Wire) permission {
	t.Helper()
	for {
		m := w.Recv()
		if m.Method == acp.ClientMethodSessionUpdate {
			continue
		}
		require.Equal(t, acp.ClientMethodSessionRequestPermission, m.Method)
		var p permission
		p.id = string(m.ID)
		require.NoError(t, json.Unmarshal(m.Params, &p.request))
		return p
	}
}

func selected(id, option string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"result":{"outcome":{"outcome":"selected","optionId":"` + option + `"}}}`
}

// optionOfKind returns the id of the offered option of a kind.
func optionOfKind(t *testing.T, p permission, kind acp.PermissionOptionKind) string {
	t.Helper()
	for _, o := range p.request.Options {
		if o.Kind == kind {
			return string(o.OptionId)
		}
	}
	require.Failf(t, "no option", "kind %s was not offered", kind)
	return ""
}

func TestAskModeAsksBeforeAToolThatWritesOrExecutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type call struct {
			tool   string
			effect tools.Effect
		}
		calls := []call{
			{"read", tools.ReadOnly},
			{"shell", tools.Executes}, // allowed once
			{"shell", tools.Executes}, // rejected
			{"write", tools.Writes},   // allowed always
			{"write", tools.Writes},   // not asked again
		}
		permitted := make(chan []bool, 1)
		w := serve(t, func(ctx context.Context, turn loop.Turn) (loop.Result, error) {
			var got []bool
			for _, c := range calls {
				title := "run " + c.tool
				ok, err := turn.Permit(ctx, c.tool, c.effect, acp.ToolCallUpdate{ToolCallId: "call-1", Title: &title})
				require.NoError(t, err)
				got = append(got, ok)
			}
			permitted <- got
			return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
		})
		id := string(create(t, w, `{}`, `,"_meta":{"acpharness":{"mode":"ask"}}`).SessionId)

		w.Send(prompt(`1`, id, "go"))
		p := awaitPermission(t, w)
		require.Equal(t, id, string(p.request.SessionId))
		require.Equal(t, "run shell", *p.request.ToolCall.Title)
		w.Send(selected(p.id, optionOfKind(t, p, acp.PermissionOptionKindAllowOnce)))
		p = awaitPermission(t, w)
		require.Equal(t, "run shell", *p.request.ToolCall.Title)
		w.Send(selected(p.id, optionOfKind(t, p, acp.PermissionOptionKindRejectOnce)))
		p = awaitPermission(t, w)
		require.Equal(t, "run write", *p.request.ToolCall.Title)
		w.Send(selected(p.id, optionOfKind(t, p, acp.PermissionOptionKindAllowAlways)))

		got, _ := w.Await(`1`)
		require.JSONEq(t, endTurn, string(got[`1`].Result))
		require.Equal(t, []bool{true, true, false, true, true}, <-permitted)

		// In auto mode nothing is asked: a request here would fail Await,
		// which accepts only the response.
		w.Send(request(`2`, "session/set_mode", `{"sessionId":"`+id+`","modeId":"auto"}`))
		w.Await(`2`)
		w.Send(prompt(`3`, id, "again"))
		got, others := w.Await(`3`)
		require.JSONEq(t, endTurn, string(got[`3`].Result))
		require.Empty(t, others)
		require.Equal(t, []bool{true, true, true, true, true}, <-permitted)
	})
}

// A cancelled turn must not leave its permission request open, whichever
// side notices first.
func TestCancelledTurnEndsItsPermissionRequest(t *testing.T) {
	asks := func(ctx context.Context, turn loop.Turn) (loop.Result, error) {
		_, err := turn.Permit(ctx, "shell", tools.Executes, acp.ToolCallUpdate{ToolCallId: "call-1"})
		if err == nil {
			return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
		}
		return loop.Result{}, err
	}

	t.Run("session/cancel withdraws the request", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := serve(t, asks)
			id := string(create(t, w, `{}`, `,"_meta":{"acpharness":{"mode":"ask"}}`).SessionId)
			w.Send(prompt(`1`, id, "go"))
			p := awaitPermission(t, w)

			w.Send(cancel(id))
			got, others := w.Await(`1`)
			require.JSONEq(t, cancelled, string(got[`1`].Result))
			require.Len(t, others, 1)
			require.Equal(t, "$/cancel_request", others[0].Method)
			require.JSONEq(t, `{"requestId":`+p.id+`}`, string(others[0].Params))
		})
	})

	t.Run("the client's cancelled outcome ends the turn as cancelled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := serve(t, asks)
			id := string(create(t, w, `{}`, `,"_meta":{"acpharness":{"mode":"ask"}}`).SessionId)
			w.Send(prompt(`1`, id, "go"))
			p := awaitPermission(t, w)

			w.Send(`{"jsonrpc":"2.0","id":` + p.id + `,"result":{"outcome":{"outcome":"cancelled"}}}`)
			got, _ := w.Await(`1`)
			require.Nil(t, got[`1`].Error)
			require.JSONEq(t, cancelled, string(got[`1`].Result))
		})
	})
}

func TestSessionNewFieldsReachTheTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		turns := make(chan loop.Turn, 1)
		w := serveWith(t, func(_ context.Context, turn loop.Turn) (loop.Result, error) {
			turns <- turn
			return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
		}, func(o *session.Options) {
			o.Supports = session.Supports{
				AdditionalDirectories: true,
				SystemPrompt:          true,
				ContextOverrides:      true,
				MCP:                   &acp.McpCapabilities{Http: true},
			}
			o.Registry = &session.Registry{}
			require.NoError(t, o.Registry.AddOverride(session.Override{
				Key: "cwd_suffix", // stands in for a key another package registers
				Parse: func(value jsontext.Value) (session.OverrideEffect, error) {
					var suffix string
					if err := json.Unmarshal(value, &suffix); err != nil {
						return session.OverrideEffect{}, err
					}
					return session.OverrideEffect{Turn: func(turn *loop.Turn) { turn.Cwd += suffix }}, nil
				},
			}))
		})

		w.Send(request(`"init"`, "initialize", `{"protocolVersion":1}`))
		got, _ := w.Await(`"init"`)
		var init acp.InitializeResponse
		require.NoError(t, json.Unmarshal(got[`"init"`].Result, &init))
		capabilities := init.AgentCapabilities
		require.NotNil(t, capabilities.SessionCapabilities.AdditionalDirectories)
		require.NotNil(t, capabilities.SessionCapabilities.SystemPrompt)
		require.True(t, capabilities.McpCapabilities.Http)

		w.Send(request(`"new"`, "session/new", `{"cwd":"/work","additionalDirectories":["/lib"],`+
			`"systemPrompt":{"mode":"append","content":[{"type":"text","text":"be brief"}]},`+
			`"mcpServers":[{"type":"http","name":"docs","url":"https://mcp.example/","headers":[]}],`+
			`"_meta":{"acpharness":{"cwd_suffix":"/sub","context_files":["notes.md"],"skills_dirs":["~/skills"]},"traceparent":"00-x"}}`))
		got, _ = w.Await(`"new"`)
		require.Nil(t, got[`"new"`].Error)
		var created acp.NewSessionResponse
		require.NoError(t, json.Unmarshal(got[`"new"`].Result, &created))

		w.Send(prompt(`1`, string(created.SessionId), "go"))
		w.Await(`1`)
		turn := <-turns
		require.Equal(t, []string{"/lib"}, turn.Dirs)
		require.Equal(t, acp.SystemPromptModeAppend, turn.SystemPrompt.Mode)
		require.Equal(t, "be brief", turn.SystemPrompt.Content[0].Text.Text)
		require.Len(t, turn.McpServers, 1)
		require.Equal(t, "docs", turn.McpServers[0].Http.Name)
		require.Equal(t, "/work/sub", turn.Cwd)
		require.Equal(t, []string{"notes.md"}, turn.ContextFiles)
		require.Equal(t, []string{"~/skills"}, turn.SkillsDirs)

		// What the wiring did not turn on, or cannot be applied, is refused.
		refused := map[string]string{
			`2`: `{"cwd":"/work","mcpServers":[],"additionalDirectories":["lib"]}`,
			`3`: `{"cwd":"/work","mcpServers":[],"systemPrompt":{"mode":"replace","content":[{"type":"text","text":"x"}]}}`,
			`4`: `{"cwd":"/work","mcpServers":[],"systemPrompt":{"mode":"append","content":[]}}`,
			`5`: `{"cwd":"/work","mcpServers":[],"systemPrompt":{"mode":"append","content":[{"type":"image","data":"","mimeType":"image/png"}]}}`,
			`6`: `{"cwd":"/work","mcpServers":[{"type":"sse","name":"docs","url":"https://mcp.example/","headers":[]}]}`,
			`7`: `{"cwd":"/work","mcpServers":[],"_meta":{"acpharness":{"model":"opus"}}}`,
			`8`: `{"cwd":"/work","mcpServers":[],"_meta":{"acpharness":{"mode":"yolo"}}}`,
			`9`:  `{"cwd":"/work","mcpServers":[],"_meta":{"acpharness":{"cwd_suffix":7}}}`,
			`10`: `{"cwd":"/work","mcpServers":[],"_meta":{"acpharness":{"context_files":[""]}}}`,
		}
		for n, params := range refused {
			w.Send(request(n, "session/new", params))
			got, _ := w.Await(n)
			require.NotNil(t, got[n].Error, params)
			require.Equal(t, acp.NewInvalidParams(nil).Code, got[n].Error.Code, params)
		}
	})
}

func TestUnsupportedSessionFieldsAndContentAreRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := serve(t, endsTurn)
		id := start(t, w)
		refused := map[string]string{
			`1`: request(`1`, "session/new", `{"cwd":"/work","mcpServers":[],"additionalDirectories":["/lib"]}`),
			`2`: request(`2`, "session/new", `{"cwd":"/work","mcpServers":[],"systemPrompt":{"mode":"append","content":[{"type":"text","text":"x"}]}}`),
			`4`: request(`4`, "session/new", `{"cwd":"/work","mcpServers":[],"_meta":{"acpharness":{"skills_dirs":["/s"]}}}`),
			`3`: request(`3`, "session/prompt", `{"sessionId":"`+id+`","prompt":[{"type":"image","data":"","mimeType":"image/png"}]}`),
		}
		for n, line := range refused {
			w.Send(line)
			got, _ := w.Await(n)
			require.NotNil(t, got[n].Error, line)
			require.Equal(t, acp.NewInvalidParams(nil).Code, got[n].Error.Code, line)
		}
	})
}

func TestImageContentReachesTheTurnWhenTheProviderTakesIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := serveWith(t, func(_ context.Context, turn loop.Turn) (loop.Result, error) {
			require.Equal(t, "image/png", turn.Prompt[0].Image.MimeType)
			return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
		}, func(o *session.Options) { o.Supports.Prompt.Image = true })
		id := start(t, w)
		w.Send(request(`1`, "session/prompt", `{"sessionId":"`+id+`","prompt":[{"type":"image","data":"","mimeType":"image/png"}]}`))
		got, _ := w.Await(`1`)
		require.JSONEq(t, endTurn, string(got[`1`].Result))
	})
}

// The model option arrives as a registration like this one.
func TestRegisteredConfigOptionIsListedSetAndAppliedToTheTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		seen := make(chan string, 1)
		w := serveWith(t, func(_ context.Context, turn loop.Turn) (loop.Result, error) {
			seen <- turn.Cwd
			return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
		}, func(o *session.Options) {
			o.Session.Mode = config.SessionModeAsk
			o.Registry = &session.Registry{}
			require.NoError(t, o.Registry.AddConfigOption(session.ConfigOption{
				ID: "model", Name: "Model", Category: acp.SessionConfigOptionCategoryModel,
				Choices: []session.Choice{{Value: "small", Name: "Small"}, {Value: "large", Name: "Large"}},
				Default: session.Value{ID: "small"},
				// Writes a field the turn has today, as the model option
				// will write the one it adds.
				Apply: func(turn *loop.Turn, v session.Value) { turn.Cwd = string(v.ID) },
			}))
			require.NoError(t, o.Registry.AddConfigOption(session.ConfigOption{ID: "fast", Name: "Fast", Boolean: true}))
		})
		// The initial values: one applies, and one names no option and is
		// ignored as the protocol says. This client takes no booleans.
		created := create(t, w, `{}`, `,"configOptions":[{"configId":"model","value":"large"},{"configId":"nope","value":"x"}]`)
		id := string(created.SessionId)
		require.Len(t, created.ConfigOptions, 2)
		require.Equal(t, acp.SessionConfigValueId("large"), created.ConfigOptions[0].Select.CurrentValue)
		require.Equal(t, session.ModeAsk, created.ConfigOptions[1].Select.CurrentValue)

		w.Send(prompt(`1`, id, "go"))
		w.Await(`1`)
		require.Equal(t, "large", <-seen)

		w.Send(
			request(`2`, "session/set_config_option", `{"sessionId":"`+id+`","configId":"model","value":"small"}`),
			request(`3`, "session/set_config_option", `{"sessionId":"`+id+`","configId":"fast","type":"boolean","value":true}`),
		)
		got, others := w.Await(`2`, `3`)
		require.Nil(t, got[`2`].Error)
		require.Equal(t, acp.NewInvalidParams(nil).Code, got[`3`].Error.Code)
		require.Empty(t, others) // not the mode, and the response has the options
		w.Send(prompt(`4`, id, "go"))
		w.Await(`4`)
		require.Equal(t, "small", <-seen)
	})
}

func TestBooleanConfigOptionForAClientThatAdvertisedIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := serveWith(t, endsTurn, func(o *session.Options) {
			o.Registry = &session.Registry{}
			require.NoError(t, o.Registry.AddConfigOption(session.ConfigOption{ID: "fast", Name: "Fast", Boolean: true}))
		})
		created := create(t, w, `{"session":{"configOptions":{"boolean":{}}}}`, "")
		require.Len(t, created.ConfigOptions, 2)
		require.False(t, created.ConfigOptions[0].Boolean.CurrentValue)

		w.Send(request(`1`, "session/set_config_option",
			`{"sessionId":"`+string(created.SessionId)+`","configId":"fast","type":"boolean","value":true}`))
		got, _ := w.Await(`1`)
		var resp acp.SetSessionConfigOptionResponse
		require.NoError(t, json.Unmarshal(got[`1`].Result, &resp))
		require.True(t, resp.ConfigOptions[0].Boolean.CurrentValue)
	})
}

func TestSlashCommands(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		toTurn := make(chan string, 1)
		w := serveWith(t, func(_ context.Context, turn loop.Turn) (loop.Result, error) {
			toTurn <- turn.Prompt[0].Text.Text
			return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
		}, func(o *session.Options) {
			o.Registry = &session.Registry{}
			// A command the turn handles itself is listed and passed on.
			require.NoError(t, o.Registry.AddCommand(session.Command{Name: "compact", Description: "Compact the conversation."}))
		})
		id := string(create(t, w, `{}`, "").SessionId)

		// /mode is answered by the session and never reaches the turn.
		w.Send(prompt(`1`, id, "/mode ask"))
		got, others := w.Await(`1`)
		require.JSONEq(t, endTurn, string(got[`1`].Result))
		updates := updatesOf(t, others)
		require.Len(t, updates, 4)
		commands := updates[0].Update.AvailableCommandsUpdate.AvailableCommands
		require.Len(t, commands, 2)
		require.Equal(t, "compact", commands[0].Name)
		require.Nil(t, commands[0].Input)
		require.Equal(t, "mode", commands[1].Name)
		require.NotEmpty(t, commands[1].Input.Unstructured.Hint)
		require.Equal(t, acp.SessionModeId("ask"), updates[1].Update.CurrentModeUpdate.CurrentModeId)
		require.Equal(t, session.ModeAsk, updates[2].Update.ConfigOptionUpdate.ConfigOptions[0].Select.CurrentValue)
		require.Contains(t, updates[3].Update.AgentMessageChunk.Content.Text.Text, "ask")

		w.Send(prompt(`2`, id, "/mode yolo"))
		got, _ = w.Await(`2`)
		require.Equal(t, acp.NewInvalidParams(nil).Code, got[`2`].Error.Code)

		// The commands are sent once. Text that names no command is a prompt.
		for n, text := range map[string]string{`3`: "/compact now", `4`: "/etc/hosts is wrong"} {
			w.Send(prompt(n, id, text))
			_, others = w.Await(n)
			require.Empty(t, others)
			require.Equal(t, text, <-toTurn)
		}
	})
}

func TestStatusOptionsAndTitle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := serveWith(t, endsTurn, func(o *session.Options) { o.Session.Mode = config.SessionModeAsk })
		id := start(t, w)

		w.Send(
			request(`1`, "session/status", `{"sessionId":"`+id+`"}`),
			request(`2`, "session/status", `{"sessionId":"nope"}`),
			request(`3`, "session/options", `{"cwd":"/elsewhere"}`),
			request(`4`, "session/options", `{"cwd":"relative"}`),
		)
		got, others := w.Await(`1`, `2`, `3`, `4`)
		require.Empty(t, others) // neither method sends anything
		require.JSONEq(t, `{"status":"open"}`, string(got[`1`].Result))
		require.Equal(t, acp.NewResourceNotFound(nil).Code, got[`2`].Error.Code)
		require.Equal(t, map[string]any{"sessionId": "nope"}, got[`2`].Error.Data)
		var options acp.UnstableSessionOptionsResponse
		require.NoError(t, json.Unmarshal(got[`3`].Result, &options))
		require.Equal(t, acp.SessionModeId("ask"), options.Modes.CurrentModeId)
		require.Equal(t, session.ModeAsk, options.ConfigOptions[0].Select.CurrentValue)
		require.Equal(t, acp.NewInvalidParams(nil).Code, got[`4`].Error.Code)

		w.Send(request(`5`, "session/set_title", `{"sessionId":"`+id+`","title":"  Fix the parser \n"}`))
		got, others = w.Await(`5`)
		require.JSONEq(t, `{"title":"Fix the parser"}`, string(got[`5`].Result))
		updates := updatesOf(t, others)
		require.Len(t, updates, 1)
		require.Equal(t, "Fix the parser", *updates[0].Update.SessionInfoUpdate.Title)

		w.Send(
			request(`6`, "session/set_title", `{"sessionId":"`+id+`","title":"   "}`),
			request(`7`, "session/set_title", `{"sessionId":"nope","title":"x"}`),
		)
		got, _ = w.Await(`6`, `7`)
		require.Equal(t, acp.NewInvalidParams(nil).Code, got[`6`].Error.Code)
		require.Equal(t, acp.NewResourceNotFound(nil).Code, got[`7`].Error.Code)
	})
}

// The methods that need the session store or the account store are not
// advertised until those exist.
func TestStoreAndAuthMethodsAreNotFound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := serve(t, endsTurn)
		id := start(t, w)
		named := `{"sessionId":"` + id + `","cwd":"/work","mcpServers":[]}`
		methods := map[string]string{
			"session/load": named, "session/resume": named, "session/list": `{}`, "session/delete": named,
			"session/fork": named, "session/archive": named, "session/unarchive": named, "logout": `{}`,
		}
		for method, params := range methods {
			w.Send(request(`1`, method, params))
			got, _ := w.Await(`1`)
			require.NotNil(t, got[`1`].Error, method)
			require.Equal(t, acp.NewMethodNotFound("").Code, got[`1`].Error.Code, method)
		}
	})
}
