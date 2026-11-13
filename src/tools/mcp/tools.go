package mcp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/valyala/bytebufferpool"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/transcript"
)

// MetaToolName is the name of the tool that reaches every server that is
// not direct.
const MetaToolName = "mcp"

// NameCollisionError is a direct tool whose name is already in use. The
// session cannot start: renaming the tool would hide it from a prompt that
// names it.
type NameCollisionError struct {
	Name string
	// Server is the server whose tool collided.
	Server string
	// With is the other server that has a tool of that name. It is empty
	// when the name belongs to a tool of the harness.
	With string
}

func (e *NameCollisionError) Error() string {
	if e.With == "" {
		return fmt.Sprintf("mcp: direct tool %q of server %q has the name of another tool of the session", e.Name, e.Server)
	}
	return fmt.Sprintf("mcp: direct tool %q of server %q has the name of a tool of server %q", e.Name, e.Server, e.With)
}

// Tools returns the tools the session's servers add: one per tool of every
// direct server, under the tool's own name, then the mcp tool when a server
// is not direct. It connects to the direct servers to read their tools, so
// it is called once, when the session starts. taken holds the names of the
// session's other tools.
//
// The direct tools are the ones the servers had at that moment. A server
// that changes its tools later does not change the session's.
func (m *Manager) Tools(ctx context.Context, taken []string) ([]tools.Tool, error) {
	// owner maps a name to the server that has it, or to "" for a name of
	// the harness.
	owner := make(map[string]string, len(taken))
	for _, name := range taken {
		owner[name] = ""
	}
	var out []tools.Tool
	var behind []Server
	for _, s := range m.servers {
		if !s.Direct {
			behind = append(behind, s)
			continue
		}
		list, err := m.ListTools(ctx, s.Name)
		if err != nil {
			return nil, err
		}
		for _, t := range list {
			if with, used := owner[t.Name]; used {
				return nil, &NameCollisionError{Name: t.Name, Server: s.Name, With: with}
			}
			owner[t.Name] = s.Name
			tool, err := m.directTool(s, t)
			if err != nil {
				return nil, err
			}
			out = append(out, tool)
		}
	}
	if len(behind) == 0 {
		return out, nil
	}
	if with, used := owner[MetaToolName]; used {
		if with == "" {
			return nil, fmt.Errorf("mcp: the %q tool has the name of another tool of the session", MetaToolName)
		}
		return nil, &NameCollisionError{Name: MetaToolName, Server: with}
	}
	return append(out, m.metaTool(behind)), nil
}

func (m *Manager) directTool(s Server, t *sdk.Tool) (tools.Tool, error) {
	if t.InputSchema == nil {
		return tools.Tool{}, fmt.Errorf("mcp: server %q: tool %q has no input schema", s.Name, t.Name)
	}
	schema, err := marshalStable(t.InputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("mcp: server %q: tool %q: input schema: %w", s.Name, t.Name, err)
	}
	title := toolTitle(t)
	return tools.Tool{
		Spec:   provider.Tool{Name: t.Name, Description: t.Description, InputSchema: schema},
		Effect: effectOf(s, t),
		Describe: func(jsontext.Value) (tools.Description, error) {
			return tools.Description{Title: s.Name + ": " + title, Kind: acp.ToolKindOther}, nil
		},
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			return m.run(ctx, s.Name, t.Name, call.Input), nil
		},
	}, nil
}

// effectOf is Executes unless the server is trusted to describe its tools
// and says the tool changes nothing. The annotations are hints a server
// writes about itself.
func effectOf(s Server, t *sdk.Tool) tools.Effect {
	if s.TrustAnnotations && t.Annotations != nil && t.Annotations.ReadOnlyHint {
		return tools.ReadOnly
	}
	return tools.Executes
}

func toolTitle(t *sdk.Tool) string {
	switch {
	case t.Title != "":
		return t.Title
	case t.Annotations != nil && t.Annotations.Title != "":
		return t.Annotations.Title
	default:
		return t.Name
	}
}

// marshalStable encodes v with object members in sorted order. A schema
// arrives as a map, and the prompt cache needs the same bytes every time.
func marshalStable(v any) (jsontext.Value, error) {
	return json.Marshal(v, json.Deterministic(true))
}

// sdkTransportRejected is the code the SDK gives an error its own HTTP
// transport made, as opposed to one a server sent: ErrRejected in
// internal/jsonrpc2/wire.go of go-sdk v1.8.0, which the SDK does not
// export. It covers a request that failed on the way and a status such as
// 502 from something in front of the server.
const sdkTransportRejected = -32005

// run calls a tool and turns what happened into a result for the model.
func (m *Manager) run(ctx context.Context, server, tool string, arguments jsontext.Value) tools.Result {
	res, err := m.CallTool(ctx, server, tool, arguments)
	if err != nil {
		if _, ok := errors.AsType[*LoginRequiredError](err); ok {
			return textResult(transcript.OutcomeFailed, fmt.Sprintf("MCP server %q needs an OAuth login, which only the user can do. The call to %q did not run.", server, tool))
		}
		if notSent, ok := errors.AsType[*notSentError](err); ok {
			return textResult(transcript.OutcomeFailed, fmt.Sprintf("The call to %q was not sent: %v", tool, notSent.err))
		}
		if refused, ok := errors.AsType[*jsonrpc.Error](err); ok && refused.Code != sdkTransportRejected {
			return textResult(transcript.OutcomeFailed, fmt.Sprintf("MCP server %q refused the call to %q: %s (code %d)", server, tool, refused.Message, refused.Code))
		}
		if callErr, ok := errors.AsType[*CallError](err); ok {
			// The call was sent. It is not sent again.
			return textResult(transcript.OutcomeUnknown, fmt.Sprintf("The call to %q on MCP server %q got no result, and it may or may not have taken effect: %v", tool, server, callErr.Err))
		}
		return textResult(transcript.OutcomeFailed, fmt.Sprintf("The call to %q was not sent: %v", tool, err))
	}
	if res.NeedsInput() {
		return textResult(transcript.OutcomeFailed, fmt.Sprintf("MCP server %q asked for more input before running %q, which this harness does not support.", server, tool))
	}
	parts, err := resultParts(res)
	if err != nil {
		return textResult(transcript.OutcomeFailed, fmt.Sprintf("MCP server %q answered the call to %q with content that could not be read: %v", server, tool, err))
	}
	outcome := transcript.OutcomeCompleted
	if res.IsError {
		outcome = transcript.OutcomeFailed
	}
	return tools.Result{Outcome: outcome, Parts: parts}
}

func textResult(outcome transcript.Outcome, text string) tools.Result {
	return tools.Result{Outcome: outcome, Parts: []transcript.Part{{Text: text}}}
}

// resultParts converts a result's content. A result that has only
// structured content is given as its JSON text.
func resultParts(res *sdk.CallToolResult) ([]transcript.Part, error) {
	parts := make([]transcript.Part, 0, len(res.Content))
	for _, content := range res.Content {
		switch c := content.(type) {
		case *sdk.TextContent:
			parts = append(parts, transcript.Part{Text: c.Text})
		case *sdk.ImageContent:
			parts = append(parts, transcript.Part{Image: &transcript.Blob{MimeType: c.MIMEType, Data: c.Data}})
		case *sdk.AudioContent:
			parts = append(parts, transcript.Part{Audio: &transcript.Blob{MimeType: c.MIMEType, Data: c.Data}})
		case *sdk.ResourceLink:
			parts = append(parts, transcript.Part{Link: &transcript.Link{URI: c.URI, Name: c.Name, MimeType: c.MIMEType}})
		case *sdk.EmbeddedResource:
			if c.Resource == nil {
				return nil, errors.New("an embedded resource has no resource")
			}
			parts = append(parts, transcript.Part{Resource: &transcript.Resource{
				URI: c.Resource.URI, MimeType: c.Resource.MIMEType, Text: c.Resource.Text, Data: c.Resource.Blob,
			}})
		default:
			return nil, fmt.Errorf("content of type %T is not valid in a tool result", content)
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		text, err := marshalStable(res.StructuredContent)
		if err != nil {
			return nil, fmt.Errorf("structured content: %w", err)
		}
		parts = append(parts, transcript.Part{Text: string(text)})
	}
	return parts, nil
}

// Actions of the mcp tool.
const (
	actionList   = "list"
	actionSearch = "search"
	actionCall   = "call"
)

const metaDescription = `Use tools of the connected MCP servers. Their schemas are not in this prompt: search for a tool to get its schema, then call it.

- action "list": the servers.
- action "search": tools whose name or description contains every word of "query", with their input schemas. "server" limits the search to one server. An empty query matches every tool.
- action "call": run "tool" of "server" with "arguments", an object that fits the tool's input schema.`

const metaSchema = `{"type":"object","properties":{"action":{"type":"string","enum":["list","search","call"]},"server":{"type":"string","description":"Server name. Required for call, optional for search."},"query":{"type":"string","description":"Words to search tool names and descriptions for."},"tool":{"type":"string","description":"Tool name, for call."},"arguments":{"type":"object","description":"The tool's arguments, for call."}},"required":["action"],"additionalProperties":false}`

type metaInput struct {
	Action    string         `json:"action"`
	Server    string         `json:"server"`
	Query     string         `json:"query"`
	Tool      string         `json:"tool"`
	Arguments jsontext.Value `json:"arguments"`
}

// metaTool is the mcp tool over the servers that are not direct.
func (m *Manager) metaTool(servers []Server) tools.Tool {
	has := func(name string) bool {
		return slices.ContainsFunc(servers, func(s Server) bool { return s.Name == name })
	}
	return tools.Tool{
		Spec: provider.Tool{Name: MetaToolName, Description: metaDescription, InputSchema: jsontext.Value(metaSchema)},
		// A call can do whatever the called tool does, and the effect is
		// fixed per tool, not per call.
		Effect: tools.Executes,
		Describe: func(input jsontext.Value) (tools.Description, error) {
			var in metaInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tools.Description{}, err
			}
			switch in.Action {
			case actionCall:
				return tools.Description{Title: in.Server + ": " + in.Tool, Kind: acp.ToolKindOther}, nil
			case actionSearch:
				return tools.Description{Title: "Search MCP tools: " + in.Query, Kind: acp.ToolKindSearch}, nil
			default:
				return tools.Description{Title: "List MCP servers", Kind: acp.ToolKindSearch}, nil
			}
		},
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			var in metaInput
			if err := json.Unmarshal(call.Input, &in); err != nil {
				return textResult(transcript.OutcomeFailed, fmt.Sprintf("The input is not valid: %v", err)), nil
			}
			if in.Server != "" && !has(in.Server) {
				return textResult(transcript.OutcomeFailed, fmt.Sprintf("There is no MCP server named %q. Use action \"list\".", in.Server)), nil
			}
			switch in.Action {
			case actionList:
				return m.metaList(servers)
			case actionSearch:
				if in.Server != "" {
					return m.metaSearch(ctx, []string{in.Server}, in.Query)
				}
				names := make([]string, len(servers))
				for i, s := range servers {
					names[i] = s.Name
				}
				return m.metaSearch(ctx, names, in.Query)
			case actionCall:
				if in.Server == "" || in.Tool == "" {
					return textResult(transcript.OutcomeFailed, `Action "call" needs "server" and "tool".`), nil
				}
				if len(in.Arguments) > 0 && in.Arguments.Kind() != '{' {
					return textResult(transcript.OutcomeFailed, `"arguments" must be an object.`), nil
				}
				return m.run(ctx, in.Server, in.Tool, in.Arguments), nil
			default:
				return textResult(transcript.OutcomeFailed, fmt.Sprintf("Unknown action %q. Use \"list\", \"search\" or \"call\".", in.Action)), nil
			}
		},
	}
}

type listedServer struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	// Connected is false for a server that has not been used yet or was
	// closed for being idle. Using it connects it.
	Connected bool `json:"connected"`
}

func (m *Manager) metaList(servers []Server) (tools.Result, error) {
	out := struct {
		Servers []listedServer `json:"servers"`
	}{Servers: make([]listedServer, len(servers))}
	for i, s := range servers {
		out.Servers[i] = listedServer{Name: s.Name, Transport: s.Transport, Connected: m.Connected(s.Name)}
	}
	return jsonResult(out)
}

type foundTool struct {
	Server      string `json:"server"`
	Name        string `json:"name"`
	Description string `json:"description,omitzero"`
	InputSchema any    `json:"input_schema"`
}

type toolRef struct {
	Server string `json:"server"`
	Name   string `json:"name"`
}

type searchOutput struct {
	Tools []foundTool `json:"tools"`
	// More are further matches, without their schemas. Search for a name
	// to get its schema.
	More []toolRef `json:"more,omitzero"`
	// Unavailable are servers that could not be searched.
	Unavailable []serverFailure `json:"unavailable,omitzero"`
}

type serverFailure struct {
	Server string `json:"server"`
	Error  string `json:"error"`
}

// metaSearch matches tools whose name or description holds every word of
// the query, without regard to case. Tools whose name holds a word come
// first. The first mcp.search_results come with their schemas.
func (m *Manager) metaSearch(ctx context.Context, servers []string, query string) (tools.Result, error) {
	words := strings.Fields(strings.ToLower(query))
	type match struct {
		server string
		tool   *sdk.Tool
		inName bool
	}
	var matches []match
	var out searchOutput
	for _, server := range servers {
		list, err := m.ListTools(ctx, server)
		if err != nil {
			if ctx.Err() != nil {
				return tools.Result{}, ctx.Err()
			}
			out.Unavailable = append(out.Unavailable, serverFailure{Server: server, Error: err.Error()})
			continue
		}
		for _, t := range list {
			name, description := strings.ToLower(t.Name), strings.ToLower(t.Description)
			all, inName := true, false
			for _, w := range words {
				n := strings.Contains(name, w)
				inName = inName || n
				all = all && (n || strings.Contains(description, w))
			}
			if all {
				matches = append(matches, match{server: server, tool: t, inName: inName})
			}
		}
	}
	slices.SortStableFunc(matches, func(a, b match) int {
		switch {
		case a.inName == b.inName:
			return 0
		case a.inName:
			return -1
		default:
			return 1
		}
	})
	out.Tools = []foundTool{}
	for i, found := range matches {
		if i < m.results {
			out.Tools = append(out.Tools, foundTool{
				Server: found.server, Name: found.tool.Name, Description: found.tool.Description, InputSchema: found.tool.InputSchema,
			})
			continue
		}
		out.More = append(out.More, toolRef{Server: found.server, Name: found.tool.Name})
	}
	return jsonResult(out)
}

// jsonResult encodes v as the text of a completed result.
func jsonResult(v any) (tools.Result, error) {
	buf := bytebufferpool.Get()
	defer bytebufferpool.Put(buf)
	if err := json.MarshalWrite(buf, v, json.Deterministic(true)); err != nil {
		return tools.Result{}, fmt.Errorf("mcp: encode result: %w", err)
	}
	return textResult(transcript.OutcomeCompleted, buf.String()), nil
}
