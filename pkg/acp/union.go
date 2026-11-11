package acp

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// Generated unions inspect the shape before decoding a variant. ReadValue
// validates syntax first. Values returned here are only used during that decode.
type objectValue map[string]jsontext.Value

type arrayValue []objectValue

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

func stringMember(b jsontext.Value, name string) (string, bool) {
	if b.Kind() != '{' {
		return "", false
	}
	dec := jsontext.NewDecoder(bytes.NewReader(b))
	if _, err := dec.ReadToken(); err != nil {
		return "", false
	}
	for dec.PeekKind() != '}' {
		key, err := dec.ReadToken()
		if err != nil {
			return "", false
		}
		if key.String() == name {
			if dec.PeekKind() != '"' {
				return "", false
			}
			value, err := dec.ReadToken()
			return value.String(), err == nil
		}
		if err := dec.SkipValue(); err != nil {
			return "", false
		}
	}
	return "", false
}
