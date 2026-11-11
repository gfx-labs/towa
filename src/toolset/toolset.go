// Package toolset joins the tools of one session: the dialect's, the
// extras and the MCP servers'. It holds the rules that belong to no single
// tool. It imports no dialect: the caller passes the dialect's tools in.
package toolset

import (
	"context"
	"fmt"
	"slices"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/repair"
)

// Extra is one extra and the tools it adds.
type Extra struct {
	// Name is the name the extra is disabled by.
	Name  string
	Tools []tools.Tool
}

// Input is what one session's tool set is made from.
type Input struct {
	// Dialect is "claude", "codex" or "generic". It names the set and is
	// not looked up.
	Dialect string
	// Tools returns the dialect's tools, in the order the model is told
	// about them.
	Tools func() ([]tools.Tool, error)
	// Extras follow the dialect's tools, in the order given.
	Extras []Extra
	// Disabled are the names of the extras to leave out.
	Disabled []string
	// MCP is discovered once per session and reused across dialect changes.
	MCP []tools.Tool
	// Gate is tools.gate. When it is enabled every tool's schema gets the
	// member that asks for a withheld result.
	Gate config.ToolsGate
}

// Result is a session's tools.
type Result struct {
	Set tools.Set
	// Schemas is the compiled input schema of every tool that has one, by
	// tool name, as the loop's dispatcher takes it. A tool with only a
	// native definition has none.
	Schemas map[string]*repair.Schema
}

// CollisionError is two tools with one name. Renaming either would hide it
// from a prompt that names it, so the session cannot start.
type CollisionError struct {
	Name string
	// First and Second say where the two tools come from: "dialect", or
	// "extra " and the extra's name.
	First, Second string
}

func (e *CollisionError) Error() string {
	return fmt.Sprintf("toolset: tool %q of %s has the name of a tool of %s", e.Name, e.Second, e.First)
}

// UnknownExtraError is a disabled extra that does not exist.
type UnknownExtraError struct {
	Name string
	// Known are the names that exist.
	Known []string
}

func (e *UnknownExtraError) Error() string {
	return fmt.Sprintf("toolset: disabled extra %q does not exist (extras: %v)", e.Name, e.Known)
}

// SchemaError is a tool whose input schema cannot be used.
type SchemaError struct {
	Tool string
	Err  error
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("toolset: input schema of tool %q: %v", e.Tool, e.Err)
}

func (e *SchemaError) Unwrap() error { return e.Err }

const fromDialect = "dialect"

// Assemble returns the session's tools: the dialect's, then the extras that
// are not disabled, then the frozen MCP tools. MCP discovery belongs to the
// session owner, not to dialect assembly.
func Assemble(ctx context.Context, in Input) (Result, error) {
	if in.Tools == nil {
		return Result{}, fmt.Errorf("toolset: dialect %q has no tools function", in.Dialect)
	}
	known := make([]string, len(in.Extras))
	for i, e := range in.Extras {
		known[i] = e.Name
	}
	for _, name := range in.Disabled {
		if !slices.Contains(known, name) {
			return Result{}, &UnknownExtraError{Name: name, Known: known}
		}
	}

	set := tools.Set{Dialect: in.Dialect}
	// owner maps a tool's name to where the tool comes from.
	owner := map[string]string{}
	add := func(from string, list []tools.Tool) error {
		for _, t := range list {
			if t.Spec.Name == "" {
				return fmt.Errorf("toolset: a tool of %s has no name", from)
			}
			if first, used := owner[t.Spec.Name]; used {
				return &CollisionError{Name: t.Spec.Name, First: first, Second: from}
			}
			owner[t.Spec.Name] = from
			set.Tools = append(set.Tools, t)
		}
		return nil
	}

	list, err := in.Tools()
	if err != nil {
		return Result{}, fmt.Errorf("toolset: dialect %q: %w", in.Dialect, err)
	}
	if err := add(fromDialect, list); err != nil {
		return Result{}, err
	}
	for _, e := range in.Extras {
		if slices.Contains(in.Disabled, e.Name) {
			continue
		}
		if err := add("extra "+e.Name, e.Tools); err != nil {
			return Result{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := add("mcp", in.MCP); err != nil {
		return Result{}, err
	}

	schemas := map[string]*repair.Schema{}
	for i := range set.Tools {
		spec := &set.Tools[i].Spec
		// A tool with only a native definition has no schema to validate
		// against or to add a member to.
		if len(spec.InputSchema) == 0 {
			continue
		}
		if in.Gate.Enabled {
			gated, err := withAcceptMember(spec.InputSchema)
			if err != nil {
				return Result{}, &SchemaError{Tool: spec.Name, Err: err}
			}
			spec.InputSchema = gated
		}
		schema, err := repair.Compile(spec.InputSchema)
		if err != nil {
			return Result{}, &SchemaError{Tool: spec.Name, Err: err}
		}
		schemas[spec.Name] = schema
	}
	return Result{Set: set, Schemas: schemas}, nil
}
