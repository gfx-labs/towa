package skill_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/agentctx"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/tools/skill"
)

func write(t *testing.T, path, body string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// listed writes two skills, one with a file beside it, and lists them the
// way a session does.
func listed(t *testing.T) (dir string, skills []agentctx.Skill) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dir = filepath.Join(base, "skills")
	write(t, filepath.Join(dir, "deploy", "SKILL.md"),
		"---\nname: deploy\ndescription: Ship a release\n---\n# Deploy\n\nRun `scripts/ship.sh`.\n")
	write(t, filepath.Join(dir, "deploy", "scripts", "ship.sh"), "#!/bin/sh\necho shipped\n")
	write(t, filepath.Join(dir, "group", "audit", "SKILL.md"),
		"---\nname: audit\ndescription: Audit tests\n---\naudit body\n")
	skills, err = agentctx.ListSkills(agentctx.Request{
		Config:  config.Context{SkillsDirs: []string{dir}},
		WorkDir: base,
	})
	require.NoError(t, err)
	require.Len(t, skills, 2)
	return dir, skills
}

func TestLoadReturnsTheSkillsTextAndItsDirectory(t *testing.T) {
	dir, skills := listed(t)
	ctx := context.Background()

	got, err := skill.Load(ctx, skills, 0, skill.Input{Name: "deploy"})
	require.NoError(t, err)
	require.Equal(t, "deploy", got.Name)
	require.Equal(t, "Ship a release", got.Description)
	require.Equal(t, filepath.Join(dir, "deploy"), got.Dir)
	require.Equal(t, filepath.Join(dir, "deploy", "SKILL.md"), got.Path)
	require.Contains(t, got.Text, "Run `scripts/ship.sh`.")
	require.NotContains(t, got.Text, "description:")
	// The text names a file relative to the directory that came with it.
	require.FileExists(t, filepath.Join(got.Dir, "scripts", "ship.sh"))

	// A skill in a nested directory, and an edit made after the listing.
	write(t, filepath.Join(dir, "group", "audit", "SKILL.md"),
		"---\nname: audit\ndescription: Audit tests\n---\nedited body\n")
	got, err = skill.Load(ctx, skills, 0, skill.Input{Name: "audit"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "group", "audit"), got.Dir)
	require.Contains(t, got.Text, "edited body")
}

func TestLoadRefusesWhatItCannotLoad(t *testing.T) {
	dir, skills := listed(t)
	ctx := context.Background()
	var serr *skill.Error

	_, err := skill.Load(ctx, skills, 0, skill.Input{Name: "nosuch"})
	require.ErrorAs(t, err, &serr)
	require.Equal(t, skill.KindUnknown, serr.Kind)
	require.Equal(t, "nosuch", serr.Name)
	require.Equal(t, []string{"deploy", "audit"}, serr.Available)

	// A name is not a path: a skill cannot be reached by its directory.
	_, err = skill.Load(ctx, skills, 0, skill.Input{Name: "group/audit"})
	require.ErrorAs(t, err, &serr)
	require.Equal(t, skill.KindUnknown, serr.Kind)

	_, err = skill.Load(ctx, skills, 0, skill.Input{})
	require.ErrorAs(t, err, &serr)
	require.Equal(t, skill.KindEmptyName, serr.Kind)

	path := filepath.Join(dir, "deploy", "SKILL.md")
	info, err := os.Stat(path)
	require.NoError(t, err)
	_, err = skill.Load(ctx, skills, info.Size()-1, skill.Input{Name: "deploy"})
	require.ErrorAs(t, err, &serr)
	require.Equal(t, skill.KindTooLarge, serr.Kind)
	require.Equal(t, info.Size(), serr.Size)
	require.Equal(t, info.Size()-1, serr.Limit)
	// At the limit exactly it loads.
	_, err = skill.Load(ctx, skills, info.Size(), skill.Input{Name: "deploy"})
	require.NoError(t, err)

	// A skill that grew past the limit after it was listed is refused too.
	write(t, path, "---\nname: deploy\ndescription: Ship a release\n---\n"+strings.Repeat("x", 4096))
	_, err = skill.Load(ctx, skills, info.Size(), skill.Input{Name: "deploy"})
	require.ErrorAs(t, err, &serr)
	require.Equal(t, skill.KindTooLarge, serr.Kind)

	// A skill whose file is gone is the harness's failure, not the model's.
	require.NoError(t, os.Remove(filepath.Join(dir, "group", "audit", "SKILL.md")))
	_, err = skill.Load(ctx, skills, 0, skill.Input{Name: "audit"})
	require.Error(t, err)
	require.NotErrorAs(t, err, &serr)
}
