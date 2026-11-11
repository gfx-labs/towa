// Package repair fixes the few ways a model is known to get a tool's
// argument wrong, and then validates the argument against the tool's
// schema. Nothing is fixed silently: every repair is returned.
package repair

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// Kind names one repair.
type Kind string

// The repairs. There are no others.
const (
	// StringToArray: a list arrived as a string holding its JSON.
	StringToArray Kind = "string_to_array"
	// StringToObject: an object arrived as a string holding its JSON.
	StringToObject Kind = "string_to_object"
	// ObjectToArray: one object arrived where a list is declared. It became
	// a list of that one object.
	ObjectToArray Kind = "object_to_array"
	// StringToNumber: a number arrived as a string.
	StringToNumber Kind = "string_to_number"
	// StringToBoolean: true or false arrived as a string.
	StringToBoolean Kind = "string_to_boolean"
)

// Repair is one change made to an argument.
type Repair struct {
	Kind Kind
	// Path is the JSON Pointer of the value that was changed. The whole
	// argument is "".
	Path string
}

// Reason says why an argument was refused.
type Reason string

// Reasons of an InvalidError.
const (
	// NotJSON: the argument is not one JSON value.
	NotJSON Reason = "not_json"
	// SchemaMismatch: the argument does not fit the tool's schema.
	SchemaMismatch Reason = "schema_mismatch"
)

// InvalidError is an argument the model has to correct. Err says what is
// wrong with it.
type InvalidError struct {
	Reason Reason
	Err    error
}

func (e *InvalidError) Error() string { return fmt.Sprintf("repair: %s: %v", e.Reason, e.Err) }

func (e *InvalidError) Unwrap() error { return e.Err }

// Schema is a tool's input schema, ready to check arguments against.
type Schema struct {
	root     *jsonschema.Schema
	resolved *jsonschema.Resolved
}

// Compile reads a tool's input schema. Draft 7 and draft 2020-12 are
// understood. A reference to another document is an error.
func Compile(schema jsontext.Value) (*Schema, error) {
	root := new(jsonschema.Schema)
	if err := json.Unmarshal(schema, root); err != nil {
		return nil, fmt.Errorf("repair: tool schema: %w", err)
	}
	resolved, err := root.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("repair: tool schema: %w", err)
	}
	return &Schema{root: root, resolved: resolved}, nil
}

// Apply repairs input when repairs are on, then validates it. It returns
// the argument to run the tool with and the repairs that were made. An
// argument with no repairs is returned byte for byte. An argument that
// still does not fit the schema is an *InvalidError.
func (s *Schema) Apply(input jsontext.Value, repairs bool) (jsontext.Value, []Repair, error) {
	root, err := parse(input)
	if err != nil {
		return nil, nil, &InvalidError{Reason: NotJSON, Err: err}
	}
	var made []Repair
	if repairs {
		w := walker{root: s.root}
		root = w.fix(s.root, root, "")
		made = w.made
	}
	output := input
	if len(made) > 0 {
		var buf bytes.Buffer
		if err := root.write(jsontext.NewEncoder(&buf)); err != nil {
			return nil, nil, fmt.Errorf("repair: encode: %w", err)
		}
		output = bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	}
	var instance any
	if err := json.Unmarshal(output, &instance); err != nil {
		return nil, nil, &InvalidError{Reason: NotJSON, Err: err}
	}
	if err := s.resolved.Validate(instance); err != nil {
		return nil, made, &InvalidError{Reason: SchemaMismatch, Err: err}
	}
	return output, made, nil
}

// node is a JSON value. Scalars keep their text and objects keep the order
// of their members, so a repaired argument differs from what the model sent
// only where it was repaired.
type node struct {
	kind    jsontext.Kind
	raw     jsontext.Value
	names   []string
	members []*node
}

func parse(data []byte) (*node, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	n, err := read(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.ReadToken(); err == nil {
		return nil, errors.New("more than one JSON value")
	}
	return n, nil
}

func read(dec *jsontext.Decoder) (*node, error) {
	switch kind := dec.PeekKind(); kind {
	case '{', '[':
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		n := &node{kind: kind}
		for dec.PeekKind() != kind+2 { // '}' follows '{' and ']' follows '[' by two.
			if kind == '{' {
				name, err := dec.ReadToken()
				if err != nil {
					return nil, err
				}
				n.names = append(n.names, name.String())
			}
			member, err := read(dec)
			if err != nil {
				return nil, err
			}
			n.members = append(n.members, member)
		}
		_, err := dec.ReadToken()
		return n, err
	default:
		raw, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		return &node{kind: raw.Kind(), raw: raw.Clone()}, nil
	}
}

func (n *node) write(enc *jsontext.Encoder) error {
	switch n.kind {
	case '{', '[':
		begin, end := jsontext.BeginObject, jsontext.EndObject
		if n.kind == '[' {
			begin, end = jsontext.BeginArray, jsontext.EndArray
		}
		if err := enc.WriteToken(begin); err != nil {
			return err
		}
		for i, member := range n.members {
			if n.kind == '{' {
				if err := enc.WriteToken(jsontext.String(n.names[i])); err != nil {
					return err
				}
			}
			if err := member.write(enc); err != nil {
				return err
			}
		}
		return enc.WriteToken(end)
	default:
		return enc.WriteValue(n.raw)
	}
}

// typeName returns the JSON Schema type a node of this kind has. An integer
// is a number here: validation tells them apart.
func typeName(kind jsontext.Kind) string {
	switch kind {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case '0':
		return "number"
	case 't', 'f':
		return "boolean"
	default:
		return "null"
	}
}

type walker struct {
	root *jsonschema.Schema
	made []Repair
}

// fix repairs n where its type is one the schema does not take, then does
// the same for what n holds.
func (w *walker) fix(schema *jsonschema.Schema, n *node, path string) *node {
	n = w.fixType(schema, n, path)
	switch n.kind {
	case '{':
		for i, name := range n.names {
			if child := w.property(schema, name); child != nil {
				n.members[i] = w.fix(child, n.members[i], path+"/"+escape(name))
			}
		}
	case '[':
		for i := range n.members {
			if child := w.item(schema, i); child != nil {
				n.members[i] = w.fix(child, n.members[i], fmt.Sprintf("%s/%d", path, i))
			}
		}
	}
	return n
}

func (w *walker) fixType(schema *jsonschema.Schema, n *node, path string) *node {
	takes := w.types(schema, 0)
	if takes == nil || takes[typeName(n.kind)] {
		// The schema does not say, or the value is already of a type it
		// takes. Whatever else is wrong is for validation to report.
		return n
	}
	if n.kind == '"' {
		var text string
		if err := json.Unmarshal(n.raw, &text); err != nil {
			return n
		}
		inner, err := parse([]byte(text))
		if err != nil {
			return n
		}
		switch inner.kind {
		case '[':
			if takes["array"] {
				w.made = append(w.made, Repair{Kind: StringToArray, Path: path})
				return inner
			}
		case '{':
			// An object is also taken where a list is declared: the next
			// step makes it a list.
			if takes["object"] || takes["array"] {
				w.made = append(w.made, Repair{Kind: StringToObject, Path: path})
				n = inner
			}
		case '0':
			if takes["number"] {
				w.made = append(w.made, Repair{Kind: StringToNumber, Path: path})
				return inner
			}
		case 't', 'f':
			if takes["boolean"] {
				w.made = append(w.made, Repair{Kind: StringToBoolean, Path: path})
				return inner
			}
		}
	}
	if n.kind == '{' && !takes["object"] && takes["array"] {
		w.made = append(w.made, Repair{Kind: ObjectToArray, Path: path})
		return &node{kind: '[', members: []*node{n}}
	}
	return n
}

// maxDepth bounds how far references and combinators are followed, so a
// schema that refers to itself cannot loop. Tool schemas nest a reference
// or an anyOf once or twice: past this depth the schema is taken to say
// nothing, which means no repair, and validation still decides.
const maxDepth = 16

// deref follows local references.
func (w *walker) deref(schema *jsonschema.Schema, depth int) *jsonschema.Schema {
	for ; schema != nil && schema.Ref != "" && depth < maxDepth; depth++ {
		name, ok := strings.CutPrefix(schema.Ref, "#/$defs/")
		defs := w.root.Defs
		if !ok {
			if name, ok = strings.CutPrefix(schema.Ref, "#/definitions/"); !ok {
				return nil
			}
			defs = w.root.Definitions
		}
		schema = defs[name]
	}
	return schema
}

// types returns the types a schema takes, or nil when it does not say.
func (w *walker) types(schema *jsonschema.Schema, depth int) map[string]bool {
	schema = w.deref(schema, depth)
	if schema == nil || depth >= maxDepth {
		return nil
	}
	names := schema.Types
	if schema.Type != "" {
		names = []string{schema.Type}
	}
	if len(names) > 0 {
		takes := map[string]bool{}
		for _, name := range names {
			if name == "integer" {
				name = "number"
			}
			takes[name] = true
		}
		return takes
	}
	// With no type of its own, a schema takes what any of its alternatives
	// takes. One alternative that does not say means the whole does not.
	if alternatives := append(append([]*jsonschema.Schema{}, schema.AnyOf...), schema.OneOf...); len(alternatives) > 0 {
		takes := map[string]bool{}
		for _, alt := range alternatives {
			sub := w.types(alt, depth+1)
			if sub == nil {
				return nil
			}
			for name := range sub {
				takes[name] = true
			}
		}
		return takes
	}
	for _, part := range schema.AllOf {
		if sub := w.types(part, depth+1); sub != nil {
			return sub
		}
	}
	return nil
}

// candidates returns the schemas that describe what a value of the given
// type holds: the schema itself, the parts of its allOf, and the one
// alternative that takes the type when exactly one does.
func (w *walker) candidates(schema *jsonschema.Schema, typ string, depth int) []*jsonschema.Schema {
	schema = w.deref(schema, depth)
	if schema == nil || depth >= maxDepth {
		return nil
	}
	out := []*jsonschema.Schema{schema}
	for _, part := range schema.AllOf {
		out = append(out, w.candidates(part, typ, depth+1)...)
	}
	var only *jsonschema.Schema
	matches := 0
	for _, alt := range append(append([]*jsonschema.Schema{}, schema.AnyOf...), schema.OneOf...) {
		if takes := w.types(alt, depth+1); takes != nil && takes[typ] {
			only = alt
			matches++
		}
	}
	if matches == 1 {
		out = append(out, w.candidates(only, typ, depth+1)...)
	}
	return out
}

func (w *walker) property(schema *jsonschema.Schema, name string) *jsonschema.Schema {
	candidates := w.candidates(schema, "object", 0)
	for _, c := range candidates {
		if child, ok := c.Properties[name]; ok {
			return child
		}
	}
	for _, c := range candidates {
		if c.AdditionalProperties != nil {
			return c.AdditionalProperties
		}
	}
	return nil
}

func (w *walker) item(schema *jsonschema.Schema, index int) *jsonschema.Schema {
	for _, c := range w.candidates(schema, "array", 0) {
		switch {
		case index < len(c.PrefixItems):
			return c.PrefixItems[index]
		case index < len(c.ItemsArray):
			return c.ItemsArray[index]
		case c.Items != nil:
			return c.Items
		}
	}
	return nil
}

// escape writes a member name as a JSON Pointer token.
func escape(name string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(name)
}
