package acp

import (
	"bytes"
	"encoding/json/jsontext"
)

// The generated union types look at a value's member names to choose a
// variant before they decode it. These helpers read the names from the raw
// value without decoding it or allocating. They are given values a
// jsontext.Decoder has already read, so the JSON is known to be valid.

// objectValue is the raw JSON of an object.
type objectValue []byte

// arrayValue is the raw JSON of an array whose items are all objects.
type arrayValue []byte

// objectMembers returns b as an object. It reports false when b is not one.
func objectMembers(b jsontext.Value) (objectValue, bool) {
	if b.Kind() != '{' {
		return nil, false
	}
	return objectValue(b), true
}

// arrayItemMembers returns b as an array of objects. It reports false when b
// is not an array or one of its items is not an object.
func arrayItemMembers(b jsontext.Value) (arrayValue, bool) {
	if b.Kind() != '[' {
		return nil, false
	}
	objects := true
	eachItem(b, func(item []byte) bool {
		objects = item[0] == '{'
		return objects
	})
	return arrayValue(b), objects
}

// has reports whether the object has a member called name.
func (o objectValue) has(name string) bool {
	return o.member(name) != nil
}

// missingOrNull reports whether the object has no member called name or has
// it with the value null.
func (o objectValue) missingOrNull(name string) bool {
	v := o.member(name)
	return v == nil || v[0] == 'n'
}

// member returns the raw value of the member called name, or nil. Like the
// decoder, it does not match a name written in another case.
func (o objectValue) member(name string) []byte {
	var found []byte
	if len(o) == 0 {
		// Not an object: a null, which decodes into a struct as nothing.
		return nil
	}
	eachMember(o, func(quoted, value []byte) bool {
		if nameIs(quoted, name) {
			found = value
		}
		return found == nil
	})
	return found
}

// allHave reports whether every item has a member for each name. It is true
// for an empty array.
func (a arrayValue) allHave(names ...string) bool {
	all := true
	eachItem(a, func(item []byte) bool {
		for _, name := range names {
			if !objectValue(item).has(name) {
				all = false
				break
			}
		}
		return all
	})
	return all
}

// stringMember returns the string value of the member of the object b called
// name. It reports false when b is not an object, has no such member, or the
// member is not a string.
func stringMember(b jsontext.Value, name string) (string, bool) {
	o, ok := objectMembers(b)
	if !ok {
		return "", false
	}
	v := o.member(name)
	if v == nil || v[0] != '"' {
		return "", false
	}
	if bytes.IndexByte(v, '\\') < 0 {
		return string(v[1 : len(v)-1]), true
	}
	s, err := jsontext.AppendUnquote(nil, v)
	return string(s), err == nil
}

// nameIs reports whether the quoted member name is name.
func nameIs(quoted []byte, name string) bool {
	if bytes.IndexByte(quoted, '\\') < 0 {
		return string(quoted[1:len(quoted)-1]) == name
	}
	s, err := jsontext.AppendUnquote(nil, quoted)
	return err == nil && string(s) == name
}

// eachMember calls fn with the quoted name and the raw value of each member
// of the object b, until fn returns false.
func eachMember(b []byte, fn func(quoted, value []byte) bool) {
	i := skipSpace(b, 1)
	for b[i] != '}' {
		nameEnd := valueEnd(b, i)
		valueStart := skipSpace(b, skipSpace(b, nameEnd)+1) // past the colon
		end := valueEnd(b, valueStart)
		if !fn(b[i:nameEnd], b[valueStart:end]) {
			return
		}
		i = skipSpace(b, end)
		if b[i] == ',' {
			i = skipSpace(b, i+1)
		}
	}
}

// eachItem calls fn with the raw value of each item of the array b, until fn
// returns false.
func eachItem(b []byte, fn func(item []byte) bool) {
	i := skipSpace(b, 1)
	for b[i] != ']' {
		end := valueEnd(b, i)
		if !fn(b[i:end]) {
			return
		}
		i = skipSpace(b, end)
		if b[i] == ',' {
			i = skipSpace(b, i+1)
		}
	}
}

func skipSpace(b []byte, i int) int {
	for b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r' {
		i++
	}
	return i
}

// valueEnd returns the index after the value that starts at b[i].
func valueEnd(b []byte, i int) int {
	switch b[i] {
	case '"':
		return stringEnd(b, i)
	case '{', '[':
		depth := 0
		for {
			switch b[i] {
			case '"':
				i = stringEnd(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
	}
	// A number, true, false or null ends at the next structural byte.
	for i < len(b) {
		switch b[i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return i
		}
		i++
	}
	return i
}

// stringEnd returns the index after the string that starts at b[i].
func stringEnd(b []byte, i int) int {
	for i++; b[i] != '"'; i++ {
		if b[i] == '\\' {
			i++
		}
	}
	return i + 1
}
