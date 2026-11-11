package toolset

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"github.com/gfx-labs/acpharness/src/tools/gate"
)

// acceptSchema is what the model is told about the gate's member. No
// upstream tool has the member. The text is jcode's, which adds the same
// member to every tool: ACCEPT_LARGE_OUTPUT_DESCRIPTION in
// crates/jcode-tool-core/src/lib.rs at 1jehuang/jcode 037163c7b. It is short
// because it is sent with every tool on every request.
const acceptSchema = `{"type":"boolean","description":"Re-run accepting the stated token cost of a withheld result."}`

// Reasons a schema cannot take the gate's member.
var (
	// ErrAcceptDeclared: the tool has its own member of that name, which
	// the gate would take out of every call before the tool saw it.
	ErrAcceptDeclared = errors.New("the schema already has a property named " + gate.AcceptMember)
	// ErrNotObject: the schema is not a JSON object, so it has no place for
	// a property.
	ErrNotObject = errors.New("the schema is not a JSON object")
)

// withAcceptMember returns schema with the gate's member added to its
// top-level properties. Everything else is kept, in its order. The member is
// not added to required.
func withAcceptMember(schema jsontext.Value) (jsontext.Value, error) {
	if schema.Kind() != '{' {
		return nil, ErrNotObject
	}
	dec := jsontext.NewDecoder(bytes.NewReader(schema))
	var out bytes.Buffer
	enc := jsontext.NewEncoder(&out)
	if _, err := dec.ReadToken(); err != nil {
		return nil, err
	}
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return nil, err
	}
	added := false
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		key := name.String()
		value, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		if key == "properties" {
			if value, err = appendProperty(value); err != nil {
				return nil, err
			}
			added = true
		}
		if err := errors.Join(enc.WriteToken(jsontext.String(key)), enc.WriteValue(value)); err != nil {
			return nil, err
		}
	}
	if !added {
		value, err := appendProperty(jsontext.Value(`{}`))
		if err != nil {
			return nil, err
		}
		if err := errors.Join(enc.WriteToken(jsontext.String("properties")), enc.WriteValue(value)); err != nil {
			return nil, err
		}
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// appendProperty adds the gate's member to the end of a properties object.
func appendProperty(properties jsontext.Value) (jsontext.Value, error) {
	if properties.Kind() != '{' {
		return nil, fmt.Errorf("properties: %w", ErrNotObject)
	}
	dec := jsontext.NewDecoder(bytes.NewReader(properties))
	var out bytes.Buffer
	enc := jsontext.NewEncoder(&out)
	if _, err := dec.ReadToken(); err != nil {
		return nil, err
	}
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return nil, err
	}
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		key := name.String()
		if key == gate.AcceptMember {
			return nil, ErrAcceptDeclared
		}
		value, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		if err := errors.Join(enc.WriteToken(jsontext.String(key)), enc.WriteValue(value)); err != nil {
			return nil, err
		}
	}
	err := errors.Join(
		enc.WriteToken(jsontext.String(gate.AcceptMember)),
		enc.WriteValue(jsontext.Value(acceptSchema)),
		enc.WriteToken(jsontext.EndObject),
	)
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}
