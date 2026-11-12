package toolset_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/httpclient"
	"github.com/gfx-labs/acpharness/src/mcp"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/gate"
	"github.com/gfx-labs/acpharness/src/tools/repair"
	"github.com/gfx-labs/acpharness/src/toolset"
)

// strict is a schema that refuses a member it does not declare, as the
// core tool schemas do.
const strict = `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`

func tool(name, schema string) tools.Tool {
	return tools.Tool{Spec: provider.Tool{Name: name, InputSchema: jsontext.Value(schema)}}
}

func core(list ...tools.Tool) func() ([]tools.Tool, error) {
	return func() ([]tools.Tool, error) { return list, nil }
}

func names(set tools.Set) []string {
	out := make([]string, len(set.Tools))
	for i, t := range set.Tools {
		out[i] = t.Spec.Name
	}
	return out
}

func gateOn(t *testing.T) config.ToolsGate {
	t.Helper()
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	require.True(t, loaded.Config.Tools.Gate.Enabled)
	return loaded.Config.Tools.Gate
}

type echoInput struct {
	Text string `json:"text"`
}

// mcpTools starts a real MCP server with one tool of the given name and
// returns the Tools method of a manager connected to it.
func mcpTools(t *testing.T, toolName string) []tools.Tool {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "toolset-test-server", Version: "1"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: toolName, Description: "Return the text it is given."},
		func(_ context.Context, _ *sdk.CallToolRequest, in echoInput) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: in.Text}}}, nil, nil
		})
	ts := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	manager, err := mcp.NewManager(mcp.Options{
		Config:        loaded.Config.MCP,
		Servers:       []mcp.Server{{Name: "remote", Transport: config.MCPTransportStreamableHTTP, URL: ts.URL, Direct: true}},
		HTTPClient:    httpclient.New(),
		ClientName:    "acpharness-test",
		ClientVersion: "0",
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	list, err := manager.Tools(t.Context(), nil)
	require.NoError(t, err)
	return list
}

func TestAssembleJoinsCoreExtrasAndMCPInOrder(t *testing.T) {
	ctx := context.Background()
	in := toolset.Input{
		Tools: core(tool("Read", strict), tool("Bash", strict)),
		Extras: []toolset.Extra{
			{Name: "webfetch", Tools: []tools.Tool{tool("webfetch", strict)}},
			{Name: "batch", Tools: []tools.Tool{tool("batch", strict)}},
		},
		Disabled: []string{"webfetch"},
		MCP:      mcpTools(t, "echo"),
		Gate:     gateOn(t),
	}
	got, err := toolset.Assemble(ctx, in)
	require.NoError(t, err)
	// The disabled extra is left out and the rest keep their order.
	require.Equal(t, []string{"Read", "Bash", "batch", "echo"}, names(got.Set))

	// Every tool, the MCP server's too, takes the gate's member, and the
	// loop's two steps accept a call that carries it: validation against
	// the compiled schema, then the gate taking the member out.
	for _, name := range names(got.Set) {
		schema := got.Schemas[name]
		require.NotNil(t, schema, name)
		input := jsontext.Value(`{"path":"a","accept_large_output":true}`)
		if name == "echo" {
			input = jsontext.Value(`{"text":"a","accept_large_output":true}`)
		}
		fixed, _, err := schema.Apply(input, false)
		require.NoError(t, err, name)
		rest, accept, err := gate.Split(fixed)
		require.NoError(t, err)
		require.True(t, accept)
		require.NotContains(t, string(rest), gate.AcceptMember)
	}
	// The model is told about the member, and it is not required.
	read, ok := got.Set.Lookup("Read")
	require.True(t, ok)
	var told struct {
		Properties map[string]jsontext.Value `json:"properties"`
		Required   []string                  `json:"required"`
	}
	require.NoError(t, json.Unmarshal(read.Spec.InputSchema, &told))
	require.Contains(t, told.Properties, gate.AcceptMember)
	require.Equal(t, []string{"path"}, told.Required)
	_, _, err = got.Schemas["Read"].Apply(jsontext.Value(`{"path":"a"}`), false)
	require.NoError(t, err)

	// With the gate off the schemas are the tools' own, so a strict schema
	// refuses the member.
	in.Gate.Enabled = false
	in.MCP = nil
	got, err = toolset.Assemble(ctx, in)
	require.NoError(t, err)
	read, _ = got.Set.Lookup("Read")
	require.Equal(t, strict, string(read.Spec.InputSchema))
	_, _, err = got.Schemas["Read"].Apply(jsontext.Value(`{"path":"a","accept_large_output":true}`), false)
	var invalid *repair.InvalidError
	require.ErrorAs(t, err, &invalid)
}

func TestAssembleRefusesASetThatCannotBeUsed(t *testing.T) {
	ctx := context.Background()
	gateCfg := gateOn(t)

	// An extra with the name of a core tool.
	_, err := toolset.Assemble(ctx, toolset.Input{
		Tools:  core(tool("Read", strict)),
		Extras: []toolset.Extra{{Name: "search", Tools: []tools.Tool{tool("Read", strict)}}},
	})
	var collision *toolset.CollisionError
	require.ErrorAs(t, err, &collision)
	require.Equal(t, toolset.CollisionError{Name: "Read", First: "core", Second: "extra search"}, *collision)

	// Disabling the extra removes the collision with it.
	got, err := toolset.Assemble(ctx, toolset.Input{
		Tools:    core(tool("Read", strict)),
		Extras:   []toolset.Extra{{Name: "search", Tools: []tools.Tool{tool("Read", strict)}}},
		Disabled: []string{"search"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"Read"}, names(got.Set))

	// Two core tools of one name.
	_, err = toolset.Assemble(ctx, toolset.Input{Tools: core(tool("Read", strict), tool("Read", strict))})
	require.ErrorAs(t, err, &collision)
	require.Equal(t, "core", collision.Second)

	// An MCP server's tool with the name of a core tool: the MCP
	// manager's own error, which names the server.
	_, err = toolset.Assemble(ctx, toolset.Input{
		Tools: core(tool("echo", strict)),
		MCP:   mcpTools(t, "echo"),
	})
	require.ErrorAs(t, err, &collision)
	require.Equal(t, "mcp", collision.Second)

	// A misspelt name in the disabled list.
	_, err = toolset.Assemble(ctx, toolset.Input{
		Tools:    core(tool("Read", strict)),
		Extras:   []toolset.Extra{{Name: "batch"}},
		Disabled: []string{"bacth"},
	})
	var unknown *toolset.UnknownExtraError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, []string{"batch"}, unknown.Known)

	// A schema that does not compile, with the gate on and off.
	var schemaErr *toolset.SchemaError
	for _, g := range []config.ToolsGate{gateCfg, {}} {
		_, err = toolset.Assemble(ctx, toolset.Input{
			Tools: core(tool("Read", strict), tool("Bad", `{"type":"object","properties":{"a":{"$ref":"#/$defs/missing"}}}`)),
			Gate:  g,
		})
		require.ErrorAs(t, err, &schemaErr)
		require.Equal(t, "Bad", schemaErr.Tool)
	}

	// A tool with its own member of the gate's name would never see it.
	own := `{"type":"object","properties":{"accept_large_output":{"type":"string"}}}`
	_, err = toolset.Assemble(ctx, toolset.Input{Tools: core(tool("Own", own)), Gate: gateCfg})
	require.ErrorAs(t, err, &schemaErr)
	require.ErrorIs(t, err, toolset.ErrAcceptDeclared)
	// With the gate off nothing takes the member out, so the tool is fine.
	_, err = toolset.Assemble(ctx, toolset.Input{Tools: core(tool("Own", own))})
	require.NoError(t, err)
}

func TestAssembleLeavesANativeToolAsItIs(t *testing.T) {
	native := tools.Tool{Spec: provider.Tool{
		Name:   "apply_patch",
		Native: jsontext.Value(`{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}`),
	}}
	got, err := toolset.Assemble(context.Background(), toolset.Input{
		Tools: core(native, tool("shell", `{"type":"object"}`)),
		Gate:  gateOn(t),
	})
	require.NoError(t, err)
	patch, ok := got.Set.Lookup("apply_patch")
	require.True(t, ok)
	require.Equal(t, native.Spec, patch.Spec)
	require.NotContains(t, got.Schemas, "apply_patch")
	// A schema with no properties gets them.
	_, _, err = got.Schemas["shell"].Apply(jsontext.Value(`{"accept_large_output":"yes"}`), false)
	var invalid *repair.InvalidError
	require.ErrorAs(t, err, &invalid)
	_, _, err = got.Schemas["shell"].Apply(jsontext.Value(`{"accept_large_output":false}`), false)
	require.NoError(t, err)
}

func TestACacheSharesSchemasBetweenSessions(t *testing.T) {
	ctx := context.Background()
	cache := &toolset.Cache{}
	assemble := func(schema string, gate config.ToolsGate) toolset.Result {
		t.Helper()
		got, err := toolset.Assemble(ctx, toolset.Input{
			// A session's tools are built anew, so the schema is a copy.
			Tools:   core(tool("Read", string([]byte(schema)))),
			MCP:     []tools.Tool{tool("echo", strict)},
			Gate:    gate,
			Schemas: cache,
		})
		require.NoError(t, err)
		return got
	}
	spec := func(r toolset.Result, name string) jsontext.Value {
		t.Helper()
		found, ok := r.Set.Lookup(name)
		require.True(t, ok)
		return found.Spec.InputSchema
	}
	gate := gateOn(t)
	first, second := assemble(strict, gate), assemble(strict, gate)
	require.Same(t, first.Schemas["Read"], second.Schemas["Read"])
	require.Same(t, &spec(first, "Read")[0], &spec(second, "Read")[0])
	// It is the schema an assembly without a cache makes.
	plain, err := toolset.Assemble(ctx, toolset.Input{Tools: core(tool("Read", strict)), Gate: gate})
	require.NoError(t, err)
	require.Equal(t, string(spec(plain, "Read")), string(spec(second, "Read")))
	// An MCP server's tool is one session's.
	require.NotSame(t, first.Schemas["echo"], second.Schemas["echo"])

	// A tool of the same name with another schema, or another gate
	// setting, gets its own.
	other := `{"type":"object","properties":{"file":{"type":"string"}},"required":["file"],"additionalProperties":false}`
	changed := assemble(other, gate)
	_, _, err = changed.Schemas["Read"].Apply(jsontext.Value(`{"file":"a","accept_large_output":true}`), false)
	require.NoError(t, err)
	ungated := assemble(other, config.ToolsGate{})
	require.Equal(t, other, string(spec(ungated, "Read")))
	_, _, err = ungated.Schemas["Read"].Apply(jsontext.Value(`{"file":"a","accept_large_output":true}`), false)
	var invalid *repair.InvalidError
	require.ErrorAs(t, err, &invalid)
}
