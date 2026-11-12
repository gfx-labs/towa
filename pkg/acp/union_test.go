package acp

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnionVariantKeepsIntegersAboveFloatPrecision(t *testing.T) {
	t.Parallel()
	// 2^53+1 has no float64, so an encoder that passes the variant through
	// untyped JSON numbers writes 2^53.
	size := 1<<53 + 1
	encoded, err := json.Marshal(ContentBlock{ResourceLink: &ContentBlockResourceLink{Name: "n", Uri: "u", Size: &size}}, wireOptions)
	require.NoError(t, err)
	var decoded ContentBlock
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, size, *decoded.ResourceLink.Size)
}
