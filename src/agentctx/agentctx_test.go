package agentctx_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/agentctx"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/provider"
)

func write(t *testing.T, path, body string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// tree is a home directory, and a git repository with a working directory
// two levels below its root. The directory above the repository also holds
// an AGENTS.md, which must never be read.
type tree struct {
	home, outside, repo, workDir string
}

func newTree(t *testing.T) tree {
	t.Helper()
	// TempDir may be reached through a symlink (macOS), and the loader
	// reports the paths it was given.
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	tr := tree{home: filepath.Join(base, "home"), outside: filepath.Join(base, "outside")}
	tr.repo = filepath.Join(tr.outside, "repo")
	tr.workDir = filepath.Join(tr.repo, "a", "b")
	require.NoError(t, os.MkdirAll(tr.workDir, 0o755))
	require.NoError(t, os.MkdirAll(tr.home, 0o755))
	out, err := exec.Command("git", "init", "--quiet", tr.repo).CombinedOutput()
	require.NoError(t, err, string(out))
	return tr
}

func (tr tree) request(dialect string, cfg config.Context) agentctx.Request {
	return agentctx.Request{Config: cfg, WorkDir: tr.workDir, HomeDir: tr.home, Dialect: dialect}
}

func paths(files []agentctx.File) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func names(blocks []provider.SystemBlock) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Name)
	}
	return out
}

func TestContextFilesAreReadFromGlobalThenRepoRootDownToWorkDir(t *testing.T) {
	tr := newTree(t)
	global := write(t, filepath.Join(tr.home, ".agents", "AGENTS.md"), "global")
	globalClaude := write(t, filepath.Join(tr.home, ".agents", "CLAUDE.md"), "global claude")
	write(t, filepath.Join(tr.outside, "AGENTS.md"), "above the repository")
	root := write(t, filepath.Join(tr.repo, "AGENTS.md"), "root")
	rootClaude := write(t, filepath.Join(tr.repo, "CLAUDE.md"), "root claude")
	mid := write(t, filepath.Join(tr.repo, "a", "AGENTS.md"), "mid")
	leafClaude := write(t, filepath.Join(tr.workDir, "CLAUDE.md"), "leaf claude")
	write(t, filepath.Join(tr.workDir, "below", "AGENTS.md"), "below the working directory")
	extra := write(t, filepath.Join(tr.home, "notes.md"), "extra")
	dirB := write(t, filepath.Join(tr.repo, "docs", "b.md"), "b")
	dirA := write(t, filepath.Join(tr.repo, "docs", "a", "z.md"), "z")
	write(t, filepath.Join(tr.repo, "docs", "skip.txt"), "not markdown")

	cfg := config.Context{
		AgentsFiles: true,
		GlobalDir:   "~/.agents",
		ExtraFiles:  []string{"~/notes.md"},
		// Relative to the working directory.
		ExtraDirs: []string{"../../docs"},
	}

	files, err := agentctx.LoadFiles(tr.request("codex", cfg))
	require.NoError(t, err)
	require.Equal(t, []string{global, root, mid, extra, dirA, dirB}, paths(files))
	require.Equal(t, "root", files[1].Text)

	files, err = agentctx.LoadFiles(tr.request(agentctx.DialectClaude, cfg))
	require.NoError(t, err)
	require.Equal(t, []string{global, globalClaude, root, rootClaude, mid, leafClaude, extra, dirA, dirB}, paths(files))

	cfg.AgentsFiles = false
	files, err = agentctx.LoadFiles(tr.request(agentctx.DialectClaude, cfg))
	require.NoError(t, err)
	require.Equal(t, []string{extra, dirA, dirB}, paths(files))
}

// Outside a repository there is no root to stop at, so only the working
// directory is searched, not every directory up to /.
func TestOutsideARepositoryOnlyTheWorkDirIsSearched(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	write(t, filepath.Join(base, "AGENTS.md"), "parent")
	leaf := write(t, filepath.Join(base, "work", "AGENTS.md"), "leaf")

	files, err := agentctx.LoadFiles(agentctx.Request{
		Config:  config.Context{AgentsFiles: true},
		WorkDir: filepath.Join(base, "work"),
	})
	require.NoError(t, err)
	require.Equal(t, []string{leaf}, paths(files))
}

// A repository whose CLAUDE.md is a symlink to AGENTS.md is common. The
// text must reach the prompt once.
func TestAFileReachedTwiceIsReadOnce(t *testing.T) {
	tr := newTree(t)
	root := write(t, filepath.Join(tr.repo, "AGENTS.md"), "root")
	require.NoError(t, os.Symlink("AGENTS.md", filepath.Join(tr.repo, "CLAUDE.md")))

	files, err := agentctx.LoadFiles(tr.request(agentctx.DialectClaude, config.Context{
		AgentsFiles: true, ExtraFiles: []string{root},
	}))
	require.NoError(t, err)
	require.Equal(t, []string{root}, paths(files))
}

func TestAFileThatCannotBeReadIsAnErrorWithItsPath(t *testing.T) {
	tr := newTree(t)

	t.Run("missing extra file", func(t *testing.T) {
		missing := filepath.Join(tr.home, "missing.md")
		_, err := agentctx.LoadFiles(tr.request("codex", config.Context{ExtraFiles: []string{missing}}))
		var pe *fs.PathError
		require.ErrorAs(t, err, &pe)
		require.Equal(t, missing, pe.Path)
		require.ErrorIs(t, err, fs.ErrNotExist)
	})
	t.Run("missing extra directory", func(t *testing.T) {
		missing := filepath.Join(tr.home, "nodir")
		_, err := agentctx.LoadFiles(tr.request("codex", config.Context{ExtraDirs: []string{missing}}))
		var pe *fs.PathError
		require.ErrorAs(t, err, &pe)
		require.Equal(t, missing, pe.Path)
	})
	t.Run("unreadable AGENTS.md", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root can read a file with no permissions")
		}
		path := write(t, filepath.Join(tr.repo, "AGENTS.md"), "root")
		require.NoError(t, os.Chmod(path, 0))
		t.Cleanup(func() { require.NoError(t, os.Remove(path)) })
		_, err := agentctx.LoadFiles(tr.request("codex", config.Context{AgentsFiles: true}))
		var pe *fs.PathError
		require.ErrorAs(t, err, &pe)
		require.Equal(t, path, pe.Path)
		require.ErrorIs(t, err, fs.ErrPermission)
	})
	t.Run("dangling CLAUDE.md symlink", func(t *testing.T) {
		link := filepath.Join(tr.workDir, "CLAUDE.md")
		require.NoError(t, os.Symlink("gone.md", link))
		t.Cleanup(func() { require.NoError(t, os.Remove(link)) })
		_, err := agentctx.LoadFiles(tr.request(agentctx.DialectClaude, config.Context{AgentsFiles: true}))
		var pe *fs.PathError
		require.ErrorAs(t, err, &pe)
		require.Equal(t, link, pe.Path)
	})
	t.Run("tilde without a home directory", func(t *testing.T) {
		req := tr.request("codex", config.Context{ExtraFiles: []string{"~/notes.md"}})
		req.HomeDir = ""
		_, err := agentctx.LoadFiles(req)
		require.Error(t, err)
	})
}

func TestSizeLimitsAreErrorsNotTruncation(t *testing.T) {
	tr := newTree(t)
	root := write(t, filepath.Join(tr.repo, "AGENTS.md"), "0123456789")
	leaf := write(t, filepath.Join(tr.workDir, "AGENTS.md"), "0123456789")

	_, err := agentctx.LoadFiles(tr.request("codex", config.Context{AgentsFiles: true, MaxFileBytes: 9}))
	var limit *agentctx.LimitError
	require.ErrorAs(t, err, &limit)
	require.Equal(t, agentctx.LimitError{Path: root, Key: "context.max_file_bytes", Size: 10, Limit: 9}, *limit)

	_, err = agentctx.LoadFiles(tr.request("codex", config.Context{AgentsFiles: true, MaxFileBytes: 10, MaxTotalBytes: 19}))
	require.ErrorAs(t, err, &limit)
	require.Equal(t, agentctx.LimitError{Path: leaf, Key: "context.max_total_bytes", Size: 20, Limit: 19}, *limit)

	files, err := agentctx.LoadFiles(tr.request("codex", config.Context{AgentsFiles: true, MaxFileBytes: 10, MaxTotalBytes: 20}))
	require.NoError(t, err)
	require.Len(t, files, 2)
}

func skillFile(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body
}

func TestSkillsAreListedAndLoadedOnDemand(t *testing.T) {
	tr := newTree(t)
	first, second := filepath.Join(tr.home, "skills"), filepath.Join(tr.repo, "skills")
	deploy := write(t, filepath.Join(first, "deploy", "SKILL.md"),
		"---\nname: deploy\nlicense: MIT\ndescription: >\n  Ship a release:\n  tag, build, publish.\n---\n# Deploy\n\nRun `scripts/ship.sh`.\n")
	// Not a skill: it is inside one.
	write(t, filepath.Join(first, "deploy", "references", "SKILL.md"), "not front matter")
	nested := write(t, filepath.Join(first, "group", "audit", "SKILL.md"), skillFile("audit", "Audit tests", "audit body"))
	write(t, filepath.Join(first, ".hidden", "x", "SKILL.md"), "not front matter")
	write(t, filepath.Join(first, "README.md"), "not a skill")
	// The same name in a later directory loses to the earlier one.
	write(t, filepath.Join(second, "deploy", "SKILL.md"), skillFile("deploy", "Another deploy", "other body"))
	// A skill linked into place is found through the link.
	linked := write(t, filepath.Join(tr.home, "elsewhere", "review", "SKILL.md"), skillFile("review", "Review a change", "review body"))
	require.NoError(t, os.Symlink(filepath.Dir(linked), filepath.Join(second, "review")))

	cfg := config.Context{SkillsDirs: []string{"~/skills", second, filepath.Join(tr.home, "no-such-dir")}}
	skills, err := agentctx.ListSkills(tr.request("codex", cfg))
	require.NoError(t, err)
	require.Equal(t, []agentctx.Skill{
		{Name: "deploy", Description: "Ship a release: tag, build, publish.", Dir: filepath.Dir(deploy), Path: deploy},
		{Name: "audit", Description: "Audit tests", Dir: filepath.Dir(nested), Path: nested},
		{Name: "review", Description: "Review a change", Dir: filepath.Join(second, "review"), Path: filepath.Join(second, "review", "SKILL.md")},
	}, skills)

	got, err := agentctx.LoadSkill(skills, "deploy", 0)
	require.NoError(t, err)
	require.Equal(t, "# Deploy\n\nRun `scripts/ship.sh`.\n", got.Body)
	require.Equal(t, filepath.Dir(deploy), got.Dir)

	// The skill is read when it is asked for, not when it is listed.
	write(t, deploy, skillFile("deploy", "x", "edited body"))
	got, err = agentctx.LoadSkill(skills, "deploy", 0)
	require.NoError(t, err)
	require.Equal(t, "edited body", got.Body)

	_, err = agentctx.LoadSkill(skills, "nope", 0)
	var unknown *agentctx.UnknownSkillError
	require.ErrorAs(t, err, &unknown)

	_, err = agentctx.LoadSkill(skills, "deploy", 5)
	var limit *agentctx.LimitError
	require.ErrorAs(t, err, &limit)
	require.Equal(t, deploy, limit.Path)
}

func TestABrokenSkillIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"no front matter":       "# Just text\n",
		"unclosed front matter": "---\nname: x\ndescription: y\n",
		"invalid yaml":          "---\nname: [x\n---\nbody\n",
		"no name":               "---\ndescription: y\n---\nbody\n",
		"no description":        "---\nname: x\n---\nbody\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := write(t, filepath.Join(dir, "s", "SKILL.md"), body)
			_, err := agentctx.ListSkills(agentctx.Request{Config: config.Context{SkillsDirs: []string{dir}}, WorkDir: dir})
			require.ErrorContains(t, err, path)
		})
	}

	t.Run("same name twice in one directory", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "a", "SKILL.md"), skillFile("dup", "one", ""))
		write(t, filepath.Join(dir, "b", "SKILL.md"), skillFile("dup", "two", ""))
		_, err := agentctx.ListSkills(agentctx.Request{Config: config.Context{SkillsDirs: []string{dir}}, WorkDir: dir})
		require.Error(t, err)
	})
}

func textPrompt(mode acp.SystemPromptMode, texts ...string) *acp.SystemPrompt {
	sp := &acp.SystemPrompt{Mode: mode}
	for _, text := range texts {
		sp.Content = append(sp.Content, acp.ContentBlock{Text: &acp.ContentBlockText{Text: text}})
	}
	return sp
}

// loadedPrompt loads a real tree with a context file, a skill and two
// configured sections, one inline and one from a file.
func loadedPrompt(t *testing.T) (tree, agentctx.Prompt) {
	t.Helper()
	tr := newTree(t)
	write(t, filepath.Join(tr.repo, "AGENTS.md"), "Use tabs.\n")
	write(t, filepath.Join(tr.home, "skills", "deploy", "SKILL.md"), skillFile("deploy", "Ship a release", "body"))
	write(t, filepath.Join(tr.home, "house.md"), "House rules.")
	loaded, err := agentctx.Load(tr.request("codex", config.Context{
		AgentsFiles: true,
		SkillsDirs:  []string{"~/skills"},
		Sections: []config.ContextSection{
			{Name: "team", Text: "Team notes."},
			{Name: "house", File: "~/house.md"},
		},
	}))
	require.NoError(t, err)
	return tr, agentctx.Prompt{
		Upstream:       "UPSTREAM",
		Tools:          "TOOLS",
		Rules:          "RULES",
		Loaded:         loaded,
		WorkDir:        tr.workDir,
		AdditionalDirs: []string{tr.home},
		SkillTool:      "skill",
	}
}

func TestSectionsComeInTheFixedOrder(t *testing.T) {
	tr, prompt := loadedPrompt(t)

	blocks, err := agentctx.Build(prompt)
	require.NoError(t, err)
	require.Equal(t, []string{
		agentctx.SectionPrompt, agentctx.SectionTools, agentctx.SectionRules, agentctx.SectionProjectContext,
		agentctx.SectionSkills, agentctx.SectionWorkDir, "team", "house",
	}, names(blocks))
	// The upstream text is passed through untouched.
	require.Equal(t, "UPSTREAM", blocks[0].Text)
	require.Contains(t, blocks[3].Text, filepath.Join(tr.repo, "AGENTS.md"))
	require.Contains(t, blocks[3].Text, "Use tabs.")
	require.Contains(t, blocks[4].Text, "deploy: Ship a release")
	require.Contains(t, blocks[5].Text, tr.workDir)
	require.Contains(t, blocks[5].Text, tr.home)
	require.Equal(t, "House rules.", blocks[7].Text)

	// A section with nothing in it is left out, not sent empty.
	blocks, err = agentctx.Build(agentctx.Prompt{Upstream: "UPSTREAM", WorkDir: tr.workDir})
	require.NoError(t, err)
	require.Equal(t, []string{agentctx.SectionPrompt, agentctx.SectionWorkDir}, names(blocks))
}

func TestClientSystemPromptAppendAndOverride(t *testing.T) {
	_, prompt := loadedPrompt(t)
	base, err := agentctx.Build(prompt)
	require.NoError(t, err)

	t.Run("append adds a last section", func(t *testing.T) {
		prompt.Client = textPrompt(acp.SystemPromptModeAppend, "Be brief.", "Answer in French.")
		blocks, err := agentctx.Build(prompt)
		require.NoError(t, err)
		require.Equal(t, base, blocks[:len(base)])
		require.Equal(t, []provider.SystemBlock{{Name: agentctx.SectionClient, Text: "Be brief.\n\nAnswer in French."}}, blocks[len(base):])
	})

	t.Run("override replaces the instruction sections only", func(t *testing.T) {
		prompt.Client = textPrompt(acp.SystemPromptModeOverride, "You are a reviewer.")
		blocks, err := agentctx.Build(prompt)
		require.NoError(t, err)
		require.Equal(t, provider.SystemBlock{Name: agentctx.SectionClient, Text: "You are a reviewer."}, blocks[0])
		// Everything that is not a default instruction is unchanged.
		var kept []provider.SystemBlock
		for _, b := range base {
			if !agentctx.IsInstructionSection(b.Name) {
				kept = append(kept, b)
			}
		}
		require.Len(t, kept, len(base)-3)
		require.Equal(t, kept, blocks[1:])
	})

	t.Run("a mode that is not supported is an error", func(t *testing.T) {
		for _, mode := range []acp.SystemPromptMode{"", "_custom", "prepend"} {
			prompt.Client = textPrompt(mode, "x")
			_, err := agentctx.Build(prompt)
			var unsupported *agentctx.UnsupportedModeError
			require.ErrorAs(t, err, &unsupported, string(mode))
			require.Equal(t, mode, unsupported.Mode)
		}
	})

	t.Run("content that is not text is an error", func(t *testing.T) {
		prompt.Client = textPrompt(acp.SystemPromptModeAppend, "x")
		prompt.Client.Content = append(prompt.Client.Content, acp.ContentBlock{Image: &acp.ContentBlockImage{}})
		_, err := agentctx.Build(prompt)
		var content *agentctx.ContentError
		require.ErrorAs(t, err, &content)
		require.Equal(t, agentctx.ContentError{Index: 1, Kind: "image"}, *content)

		prompt.Client = textPrompt(acp.SystemPromptModeOverride)
		_, err = agentctx.Build(prompt)
		require.ErrorAs(t, err, &content)
		require.Equal(t, -1, content.Index)
	})
}

func TestAConfiguredSectionCannotTakeABuiltInName(t *testing.T) {
	tr := newTree(t)
	loaded, err := agentctx.Load(tr.request("codex", config.Context{
		Sections: []config.ContextSection{{Name: agentctx.SectionRules, Text: "mine"}},
	}))
	require.NoError(t, err)
	_, err = agentctx.Build(agentctx.Prompt{Upstream: "UPSTREAM", Loaded: loaded})
	require.Error(t, err)
}
