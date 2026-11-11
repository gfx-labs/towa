package transcript_test

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/transcript"
)

func TestOpaqueCompatibilityRequiresKnownRequestOrigin(t *testing.T) {
	origin := transcript.Origin{Model: "model-a", Account: "account-a"}
	item := transcript.Item{
		Thinking: &transcript.Thinking{}, Provider: "owner", Opaque: jsontext.Value(`{}`), Origin: &origin,
	}
	for _, tc := range []struct {
		name     string
		provider string
		origin   transcript.Origin
		want     bool
	}{
		{"same request identity", "owner", origin, true},
		{"different provider", "other", origin, false},
		{"different model", "owner", transcript.Origin{Model: "model-b", Account: origin.Account}, false},
		{"different account", "owner", transcript.Origin{Model: origin.Model, Account: "account-b"}, false},
		{"unknown provider", "", origin, false},
		{"unknown target model", "owner", transcript.Origin{Account: origin.Account}, false},
		{"unknown target account", "owner", transcript.Origin{Model: origin.Model}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, item.OpaqueCompatible(tc.provider, tc.origin))
		})
	}

	for _, stored := range []*transcript.Origin{nil, {}, {Model: origin.Model}, {Account: origin.Account}} {
		legacy := item
		legacy.Origin = stored
		assert.False(t, legacy.OpaqueCompatible("owner", origin))
		assert.Same(t, stored, legacy.Origin, "compatibility must not infer provenance")
		if stored == nil {
			require.NoError(t, legacy.Validate(), "legacy plain rendering remains possible")
		} else {
			require.Error(t, legacy.Validate())
		}
	}
	plain := item
	plain.Opaque = nil
	assert.False(t, plain.OpaqueCompatible("owner", origin))
}
