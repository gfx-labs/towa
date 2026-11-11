package repair_test

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/tools/repair"
)

// schema has each shape the repairs look at: a list of objects, a nested
// object, numbers, a boolean, a string, a field that takes two types, and a
// list reached through a reference.
const schema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string"},
    "edits": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "old": {"type": "string"},
          "new": {"type": "string"},
          "all": {"type": "boolean"}
        },
        "required": ["old", "new"],
        "additionalProperties": false
      }
    },
    "options": {
      "type": "object",
      "properties": {"limit": {"type": "integer"}, "ratio": {"type": "number"}}
    },
    "limit": {"type": "integer", "minimum": 1},
    "tags": {"$ref": "#/$defs/tags"},
    "either": {"anyOf": [{"type": "string"}, {"type": "integer"}]},
    "maybe": {"type": ["integer", "null"]}
  },
  "required": ["path"],
  "additionalProperties": false,
  "$defs": {"tags": {"type": "array", "items": {"type": "string"}}}
}`

func compile(t *testing.T) *repair.Schema {
	t.Helper()
	s, err := repair.Compile(jsontext.Value(schema))
	require.NoError(t, err)
	return s
}

func TestEachListedRepairIsMadeAndReported(t *testing.T) {
	s := compile(t)
	cases := []struct {
		name  string
		input string
		want  string
		made  []repair.Repair
	}{
		{
			"a list sent as a string",
			`{"path":"a","tags":"[\"x\",\"y\"]"}`,
			`{"path":"a","tags":["x","y"]}`,
			[]repair.Repair{{Kind: repair.StringToArray, Path: "/tags"}},
		},
		{
			"an object sent as a string, and a number inside it too",
			`{"path":"a","options":"{\"limit\":\"3\",\"ratio\":0.5}"}`,
			`{"path":"a","options":{"limit":3,"ratio":0.5}}`,
			[]repair.Repair{{Kind: repair.StringToObject, Path: "/options"}, {Kind: repair.StringToNumber, Path: "/options/limit"}},
		},
		{
			"one object where a list is declared",
			`{"path":"a","edits":{"old":"x","new":"y"}}`,
			`{"path":"a","edits":[{"old":"x","new":"y"}]}`,
			[]repair.Repair{{Kind: repair.ObjectToArray, Path: "/edits"}},
		},
		{
			"one object as a string where a list is declared",
			`{"path":"a","edits":"{\"old\":\"x\",\"new\":\"y\",\"all\":\"true\"}"}`,
			`{"path":"a","edits":[{"old":"x","new":"y","all":true}]}`,
			[]repair.Repair{
				{Kind: repair.StringToObject, Path: "/edits"},
				{Kind: repair.ObjectToArray, Path: "/edits"},
				{Kind: repair.StringToBoolean, Path: "/edits/0/all"},
			},
		},
		{
			"a number and a boolean sent as strings",
			`{"path":"a","limit":"20","edits":[{"old":"x","new":"y","all":"false"}],"maybe":"7"}`,
			`{"path":"a","limit":20,"edits":[{"old":"x","new":"y","all":false}],"maybe":7}`,
			[]repair.Repair{
				{Kind: repair.StringToBoolean, Path: "/edits/0/all"},
				{Kind: repair.StringToNumber, Path: "/limit"},
				{Kind: repair.StringToNumber, Path: "/maybe"},
			},
		},
		{
			"the whole argument sent as a string",
			`"{\"path\":\"a\"}"`,
			`{"path":"a"}`,
			[]repair.Repair{{Kind: repair.StringToObject, Path: ""}},
		},
	}
	for _, c := range cases {
		got, made, err := s.Apply(jsontext.Value(c.input), true)
		require.NoError(t, err, c.name)
		require.Equal(t, c.want, string(got), c.name)
		require.ElementsMatch(t, c.made, made, c.name)

		// With repairs off the same argument is refused.
		_, made, err = s.Apply(jsontext.Value(c.input), false)
		var invalid *repair.InvalidError
		require.ErrorAs(t, err, &invalid, c.name)
		require.Empty(t, made, c.name)
	}
}

func TestAValidArgumentIsReturnedUnchanged(t *testing.T) {
	s := compile(t)
	for _, input := range []string{
		// Spacing, member order and number text are the model's own.
		`{ "limit": 1e2, "path": "a",  "edits": [ {"new":"y", "old":"x"} ] }`,
		// A string that holds JSON stays a string where a string is taken.
		`{"path":"[1,2]","either":"12","tags":["{}","true","3"]}`,
		`{"path":"a","maybe":null}`,
	} {
		got, made, err := s.Apply(jsontext.Value(input), true)
		require.NoError(t, err, input)
		require.Empty(t, made, input)
		require.Equal(t, input, string(got))
	}
}

func TestAnythingElseIsAnErrorForTheModel(t *testing.T) {
	s := compile(t)
	for name, c := range map[string]struct {
		input  string
		reason repair.Reason
	}{
		"missing required member":        {`{"limit":1}`, repair.SchemaMismatch},
		"unknown member":                 {`{"path":"a","extra":1}`, repair.SchemaMismatch},
		"number where a string is taken": {`{"path":1}`, repair.SchemaMismatch},
		"string that is not a number":    {`{"path":"a","limit":"many"}`, repair.SchemaMismatch},
		"number string below a minimum":  {`{"path":"a","limit":"0"}`, repair.SchemaMismatch},
		"fraction where an integer goes": {`{"path":"a","limit":"1.5"}`, repair.SchemaMismatch},
		"string that is not a list":      {`{"path":"a","tags":"x,y"}`, repair.SchemaMismatch},
		"string holding the wrong type":  {`{"path":"a","tags":"true"}`, repair.SchemaMismatch},
		"scalar where a list goes":       {`{"path":"a","tags":"x"}`, repair.SchemaMismatch},
		"list where an object goes":      {`{"path":"a","options":[{"limit":1}]}`, repair.SchemaMismatch},
		"null is not repaired":           {`{"path":"a","limit":"null"}`, repair.SchemaMismatch},
		"cut off":                        {`{"path":"a"`, repair.NotJSON},
		"two values":                     {`{"path":"a"} {"path":"b"}`, repair.NotJSON},
		"duplicate member":               {`{"path":"a","path":"b"}`, repair.NotJSON},
		"empty":                          {``, repair.NotJSON},
	} {
		_, _, err := s.Apply(jsontext.Value(c.input), true)
		var invalid *repair.InvalidError
		require.ErrorAs(t, err, &invalid, name)
		require.Equal(t, c.reason, invalid.Reason, name)
	}
}

func TestCompileRefusesASchemaItCannotUse(t *testing.T) {
	for _, bad := range []string{`{"type": 3}`, `{"$ref": "https://example.com/other.json"}`, `{`} {
		_, err := repair.Compile(jsontext.Value(bad))
		require.Error(t, err, bad)
	}
}
