package agentctx_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/config"
)

// The context section holds lists and a list of tables, which each file
// format writes differently. A real file must decode into the typed struct.
func TestContextSectionDecodesFromAFile(t *testing.T) {
	path := write(t, filepath.Join(t.TempDir(), "c.toml"), `
[context]
agents_files = false
global_dir = ""
extra_files = ["notes.md"]
extra_dirs = ["docs"]
skills_dirs = ["~/a", "/b"]
max_file_bytes = 1000
max_total_bytes = 5000

[[context.sections]]
name = "team"
text = "Team notes."

[[context.sections]]
name = "house"
file = "~/house.md"
`)
	got, err := config.Load(config.Sources{Files: []string{path}})
	require.NoError(t, err)
	require.Equal(t, config.Context{
		ExtraFiles:    []string{"notes.md"},
		ExtraDirs:     []string{"docs"},
		SkillsEnabled: true,
		SkillsDirs:    []string{"~/a", "/b"},
		MaxFileBytes:  1000,
		MaxTotalBytes: 5000,
		Sections: []config.ContextSection{
			{Name: "team", Text: "Team notes."},
			{Name: "house", File: "~/house.md"},
		},
	}, got.Config.Context)
}

func TestContextSectionRejectsWhatCannotBeActedOn(t *testing.T) {
	for name, body := range map[string]string{
		"negative file limit":  "[context]\nmax_file_bytes = -1\n",
		"negative total limit": "[context]\nmax_total_bytes = -1\n",
		"empty path":           "[context]\nextra_files = [\"\"]\n",
		"section without name": "[[context.sections]]\ntext = \"x\"\n",
		"section with both":    "[[context.sections]]\nname = \"a\"\ntext = \"x\"\nfile = \"y\"\n",
		"section with neither": "[[context.sections]]\nname = \"a\"\n",
		"section name twice":   "[[context.sections]]\nname = \"a\"\ntext = \"x\"\n[[context.sections]]\nname = \"a\"\ntext = \"y\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := write(t, filepath.Join(t.TempDir(), "c.toml"), body)
			_, err := config.Load(config.Sources{Files: []string{path}})
			require.Error(t, err)
		})
	}
}
