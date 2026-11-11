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
// upstream tool schemas do.
const strict = `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`

func tool(name, schema string) tools.Tool {
	return tools.Tool{Spec: provider.Tool{Name: name, InputSchema: jsontext.Value(schema)}}
}

func dialect(list ...tools.Tool) func() ([]tools.Tool, error) {
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
func mcpTools(t *testing.T, toolName string) func(context.Context, []string) ([]tools.Tool, error) {
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
	return manager.Tools
}

func TestAssembleJoinsDialectExtrasAndMCPInOrder(t *testing.T) {
	ctx := context.Background()
	in := toolset.Input{
		Dialect: "generic",
		Tools:   dialect(tool("Read", strict), tool("Bash", strict)),
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
	require.Equal(t, "generic", got.Set.Dialect)
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

	// An extra with the name of a dialect tool.
	_, err := toolset.Assemble(ctx, toolset.Input{
		Dialect: "generic",
		Tools:   dialect(tool("Read", strict)),
		Extras:  []toolset.Extra{{Name: "search", Tools: []tools.Tool{tool("Read", strict)}}},
	})
	var collision *toolset.CollisionError
	require.ErrorAs(t, err, &collision)
	require.Equal(t, toolset.CollisionError{Name: "Read", First: "dialect", Second: "extra search"}, *collision)

	// Disabling the extra removes the collision with it.
	got, err := toolset.Assemble(ctx, toolset.Input{
		Dialect:  "generic",
		Tools:    dialect(tool("Read", strict)),
		Extras:   []toolset.Extra{{Name: "search", Tools: []tools.Tool{tool("Read", strict)}}},
		Disabled: []string{"search"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"Read"}, names(got.Set))

	// Two tools of one name inside the dialect.
	_, err = toolset.Assemble(ctx, toolset.Input{Dialect: "generic", Tools: dialect(tool("Read", strict), tool("Read", strict))})
	require.ErrorAs(t, err, &collision)
	require.Equal(t, "dialect", collision.Second)

	// An MCP server's tool with the name of a dialect tool: the MCP
	// manager's own error, which names the server.
	_, err = toolset.Assemble(ctx, toolset.Input{
		Dialect: "generic",
		Tools:   dialect(tool("echo", strict)),
		MCP:     mcpTools(t, "echo"),
	})
	var mcpCollision *mcp.NameCollisionError
	require.ErrorAs(t, err, &mcpCollision)
	require.Equal(t, "remote", mcpCollision.Server)

	// A misspelt name in the disabled list.
	_, err = toolset.Assemble(ctx, toolset.Input{
		Dialect:  "generic",
		Tools:    dialect(tool("Read", strict)),
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
			Dialect: "generic",
			Tools:   dialect(tool("Read", strict), tool("Bad", `{"type":"object","properties":{"a":{"$ref":"#/$defs/missing"}}}`)),
			Gate:    g,
		})
		require.ErrorAs(t, err, &schemaErr)
		require.Equal(t, "Bad", schemaErr.Tool)
	}

	// A tool with its own member of the gate's name would never see it.
	own := `{"type":"object","properties":{"accept_large_output":{"type":"string"}}}`
	_, err = toolset.Assemble(ctx, toolset.Input{Dialect: "generic", Tools: dialect(tool("Own", own)), Gate: gateCfg})
	require.ErrorAs(t, err, &schemaErr)
	require.ErrorIs(t, err, toolset.ErrAcceptDeclared)
	// With the gate off nothing takes the member out, so the tool is fine.
	_, err = toolset.Assemble(ctx, toolset.Input{Dialect: "generic", Tools: dialect(tool("Own", own))})
	require.NoError(t, err)
}

func TestAssembleLeavesANativeToolAsItIs(t *testing.T) {
	native := tools.Tool{Spec: provider.Tool{
		Name:   "apply_patch",
		Native: jsontext.Value(`{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}`),
	}}
	got, err := toolset.Assemble(context.Background(), toolset.Input{
		Dialect: "codex",
		Tools:   dialect(native, tool("shell", `{"type":"object"}`)),
		Gate:    gateOn(t),
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
