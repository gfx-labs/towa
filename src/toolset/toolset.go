// Package toolset joins the tools of one session: the core tools, the
// extras and the MCP servers'. It holds the rules that belong to no single
// tool. It imports no tool package: the caller passes the tools in.
package toolset

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/repair"
)

// Extra is one extra and the tools it adds.
type Extra struct {
	// Name is the name the extra is disabled by.
	Name  string
	Tools []tools.Tool
}

// Input is what one session's tool set is made from.
type Input struct {
	// Tools returns the core tools, in the order the model is told about
	// them.
	Tools func() ([]tools.Tool, error)
	// Extras follow the core tools, in the order given.
	Extras []Extra
	// Disabled are the names of the extras to leave out.
	Disabled []string
	// MCP is discovered once per session.
	MCP []tools.Tool
	// Gate is tools.gate. When it is enabled every tool's schema gets the
	// member that asks for a withheld result.
	Gate config.ToolsGate
	// Schemas keeps the schemas of the core tools and the extras between
	// sessions. Nil compiles them again.
	Schemas *Cache
}

// Cache keeps the input schema of each core tool and extra as it is sent
// and as it is compiled, so that the sessions of one process share one copy.
// It holds one entry per tool name. A tool whose schema or gate setting
// differs from the entry is compiled again and replaces it. MCP tools are
// not kept: their names and schemas differ by session.
type Cache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	// source is the schema the tool came with.
	source jsontext.Value
	gated  bool
	// sent is source with the gate's member when gated.
	sent     jsontext.Value
	compiled *repair.Schema
}

// prepare returns the schema to send for a tool and its compiled form.
func prepare(source jsontext.Value, gated bool) (jsontext.Value, *repair.Schema, error) {
	sent := source
	if gated {
		var err error
		if sent, err = withAcceptMember(source); err != nil {
			return nil, nil, err
		}
	}
	compiled, err := repair.Compile(sent)
	if err != nil {
		return nil, nil, err
	}
	return sent, compiled, nil
}

func (c *Cache) prepare(name string, source jsontext.Value, gated bool) (jsontext.Value, *repair.Schema, error) {
	c.mu.Lock()
	entry, ok := c.entries[name]
	c.mu.Unlock()
	if ok && entry.gated == gated && bytes.Equal(entry.source, source) {
		return entry.sent, entry.compiled, nil
	}
	sent, compiled, err := prepare(source, gated)
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]cacheEntry{}
	}
	c.entries[name] = cacheEntry{source: source, gated: gated, sent: sent, compiled: compiled}
	return sent, compiled, nil
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
	// First and Second say where the two tools come from: "core", "mcp", or
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

const fromCore = "core"

// Assemble returns the session's tools: the core tools, then the extras that
// are not disabled, then the frozen MCP tools. MCP discovery belongs to the
// session owner, not to assembly.
func Assemble(ctx context.Context, in Input) (Result, error) {
	if in.Tools == nil {
		return Result{}, errors.New("toolset: there is no core tools function")
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

	var set tools.Set
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
		return Result{}, fmt.Errorf("toolset: core tools: %w", err)
	}
	if err := add(fromCore, list); err != nil {
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
	// shared counts the core tools and extras, which come first.
	shared := len(set.Tools)
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
		var sent jsontext.Value
		var compiled *repair.Schema
		var err error
		if in.Schemas != nil && i < shared {
			sent, compiled, err = in.Schemas.prepare(spec.Name, spec.InputSchema, in.Gate.Enabled)
		} else {
			sent, compiled, err = prepare(spec.InputSchema, in.Gate.Enabled)
		}
		if err != nil {
			return Result{}, &SchemaError{Tool: spec.Name, Err: err}
		}
		spec.InputSchema, schemas[spec.Name] = sent, compiled
	}
	return Result{Set: set, Schemas: schemas}, nil
}
