package src_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// module is the path every package of this directory is under.
const module = "github.com/gfx-labs/towa/src/"

// layers are the groups of src, lowest first, each with the groups its
// packages may import. A group imports only groups above it in this list, so
// the graph has no cycle, and a new edge between two groups fails here until
// someone decides it is wanted. Tests are held to the same list as the code
// they test.
var layers = []struct {
	group string
	may   []string
}{
	{"config", nil},
	{"transcript", nil},
	{"support", []string{"config"}},
	{"auth", []string{"config", "support"}},
	{"provider", []string{"config", "transcript", "support", "auth"}},
	// sidecar's tests record and replay with provider/providertest.
	{"sidecar", []string{"config", "support", "auth", "provider"}},
	{"store", []string{"transcript", "support"}},
	{"prompt", []string{"config", "transcript", "support", "auth", "provider"}},
	{"tools", []string{"config", "transcript", "support", "auth", "provider", "store", "prompt"}},
	{"agent", []string{"config", "transcript", "support", "auth", "provider", "sidecar", "store", "prompt", "tools"}},
	{"cli", []string{"config", "transcript", "support", "auth", "provider", "sidecar", "store", "prompt", "tools", "agent"}},
}

// Every package of src is in a group, and imports only what its group may.
func TestPackagesImportOnlyTheLayersBelowThem(t *testing.T) {
	groups := make([]string, len(layers))
	for i, layer := range layers {
		groups[i] = layer.group
		for _, lower := range layer.may {
			require.Less(t, slices.Index(groups, lower), i, "%s may import %s, which is not above it in the list", layer.group, lower)
			require.GreaterOrEqual(t, slices.Index(groups, lower), 0, "%s may import %s, which is not a group", layer.group, lower)
		}
	}

	out, err := exec.CommandContext(t.Context(), "go", "list", "-f",
		`{{.ImportPath}}|{{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}`, "./...").Output()
	require.NoError(t, err)
	group := func(pkg string) string {
		name, _, _ := strings.Cut(strings.TrimPrefix(pkg, module), "/")
		return name
	}
	for line := range strings.Lines(string(out)) {
		pkg, imports, _ := strings.Cut(strings.TrimSpace(line), "|")
		if pkg+"/" == module {
			continue
		}
		from := group(pkg)
		at := slices.Index(groups, from)
		require.GreaterOrEqual(t, at, 0, "%s is in no group: add %q to the layers, or move the package into one", pkg, from)
		for imported := range strings.FieldsSeq(imports) {
			if !strings.HasPrefix(imported, module) {
				continue
			}
			to := group(imported)
			if to == from {
				continue
			}
			require.Contains(t, layers[at].may, to, "%s imports %s: the %s group may not import the %s group", pkg, imported, from, to)
		}
	}
}
