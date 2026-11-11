package acp

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// Generated unions inspect the shape before decoding a variant. ReadValue
// validates syntax first. Values returned here are only used during that decode.
type objectValue map[string]jsontext.Value

type arrayValue []objectValue

// The old generator encoded object variants through map[string]any. Keep
// that shaping here: removing it changes integer encoding above 2^53. A
// separate protocol change can remove that loss of precision, not this
// serializer migration.
func marshalUnionObject(enc *jsontext.Encoder, value any) error {
	b, err := json.Marshal(value, enc.Options(), jsontext.EscapeForHTML(true))
	if err != nil {
		return err
	}
	var object map[string]any
	if err := json.Unmarshal(b, &object); err != nil {
		return err
	}
	return json.MarshalEncode(enc, object, json.Deterministic(true), jsontext.EscapeForHTML(true))
}

func objectMembers(b jsontext.Value) (objectValue, bool) {
	if b.Kind() != '{' {
		return nil, false
	}
	var members objectValue
	err := json.Unmarshal(b, &members)
	return members, err == nil
}

func arrayItemMembers(b jsontext.Value) (arrayValue, bool) {
	if b.Kind() != '[' {
		return nil, false
	}
	var items arrayValue
	err := json.Unmarshal(b, &items)
	if err != nil {
		return nil, false
	}
	for _, item := range items {
		if item == nil {
			return nil, false
		}
	}
	return items, true
}

func (o objectValue) has(name string) bool {
	_, ok := o[name]
	return ok
}

func (o objectValue) missingOrNull(name string) bool {
	v, ok := o[name]
	return !ok || v.Kind() == 'n'
}

func (a arrayValue) allHave(names ...string) bool {
	for _, item := range a {
		for _, name := range names {
			if !item.has(name) {
				return false
			}
		}
	}
	return true
}
