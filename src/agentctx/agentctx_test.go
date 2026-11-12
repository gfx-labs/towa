package agentctx_test

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/agentctx"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
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

func (tr tree) request(cfg config.Context) agentctx.Request {
	return agentctx.Request{Config: cfg, WorkDir: tr.workDir, HomeDir: tr.home}
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

func TestContextFilesAreReadFromDefaultsThenConfiguredEntriesInOrder(t *testing.T) {
	tr := newTree(t)
	global := write(t, filepath.Join(tr.home, "AGENTS.md"), "global")
	ruleB := write(t, filepath.Join(tr.home, ".agents", "rules", "b.md"), "rule b")
	ruleA := write(t, filepath.Join(tr.home, ".agents", "rules", "a", "z.md"), "rule z")
	write(t, filepath.Join(tr.outside, "AGENTS.md"), "above the repository")
	root := write(t, filepath.Join(tr.repo, "AGENTS.md"), "root")
	mid := write(t, filepath.Join(tr.repo, "a", "AGENTS.md"), "mid")
	leaf := write(t, filepath.Join(tr.workDir, "AGENTS.md"), "leaf")
	write(t, filepath.Join(tr.workDir, "below", "AGENTS.md"), "below the working directory")
	extra := write(t, filepath.Join(tr.home, "notes.md"), "extra")
	dirB := write(t, filepath.Join(tr.repo, "docs", "b.md"), "b")
	dirA := write(t, filepath.Join(tr.repo, "docs", "a", "z.md"), "z")
	write(t, filepath.Join(tr.repo, "docs", "skip.txt"), "not markdown")

	cfg := config.Context{
		DefaultAgents: true,
		// A directory, then a file: config order, not kind, decides. The
		// directory is relative to the working directory.
		Agents:     []string{"../../docs", "~/notes.md"},
		AgentsText: []string{"from a command, with, commas\nand lines"},
	}

	files, err := agentctx.LoadFiles(tr.request(cfg))
	require.NoError(t, err)
	require.Equal(t, []string{global, ruleA, ruleB, root, mid, leaf, dirA, dirB, extra, "context.agents_text[0]"}, paths(files))
	require.Equal(t, "root", files[3].Text)
	require.Equal(t, "from a command, with, commas\nand lines", files[9].Text)

	cfg.DefaultAgents = false
	files, err = agentctx.LoadFiles(tr.request(cfg))
	require.NoError(t, err)
	require.Equal(t, []string{dirA, dirB, extra, "context.agents_text[0]"}, paths(files))
}

// Outside a repository there is no root to stop at, so only the working
// directory is searched, not every directory up to /.
func TestOutsideARepositoryOnlyTheWorkDirIsSearched(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	write(t, filepath.Join(base, "AGENTS.md"), "parent")
	leaf := write(t, filepath.Join(base, "work", "AGENTS.md"), "leaf")

	files, err := agentctx.LoadFiles(agentctx.Request{
		Config:  config.Context{DefaultAgents: true},
		WorkDir: filepath.Join(base, "work"),
	})
	require.NoError(t, err)
	require.Equal(t, []string{leaf}, paths(files))
}

// An extra file that is also a repository's AGENTS.md, directly or through a
// symlink, must reach the prompt once.
func TestAFileReachedTwiceIsReadOnce(t *testing.T) {
	tr := newTree(t)
	root := write(t, filepath.Join(tr.repo, "AGENTS.md"), "root")
	link := filepath.Join(tr.home, "rules.md")
	require.NoError(t, os.Symlink(root, link))

	files, err := agentctx.LoadFiles(tr.request(config.Context{
		DefaultAgents: true, Agents: []string{root, link},
	}))
	require.NoError(t, err)
	require.Equal(t, []string{root}, paths(files))
}

func TestInstructionDirectoryRootMayBeASymlink(t *testing.T) {
	for _, defaults := range []bool{false, true} {
		t.Run(fmt.Sprintf("defaults=%t", defaults), func(t *testing.T) {
			tr := newTree(t)
			target := filepath.Join(tr.home, "instructions")
			write(t, filepath.Join(target, "nested", "rule.md"), "a linked rule")
			link := filepath.Join(tr.home, ".agents", "rules")
			require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
			require.NoError(t, os.Symlink(target, link))
			cfg := config.Context{DefaultAgents: defaults}
			if !defaults {
				cfg.Agents = []string{link}
			}
			files, err := agentctx.LoadFiles(tr.request(cfg))
			require.NoError(t, err)
			require.Equal(t, []string{filepath.Join(link, "nested", "rule.md")}, paths(files))
			require.Equal(t, "a linked rule", files[0].Text)
		})
	}
}

func TestAFileThatCannotBeReadIsAnErrorWithItsPath(t *testing.T) {
	tr := newTree(t)

	t.Run("missing agents file", func(t *testing.T) {
		missing := filepath.Join(tr.home, "missing.md")
		_, err := agentctx.LoadFiles(tr.request(config.Context{Agents: []string{missing}}))
		var pe *fs.PathError
		require.ErrorAs(t, err, &pe)
		require.Equal(t, missing, pe.Path)
		require.ErrorIs(t, err, fs.ErrNotExist)
	})
	t.Run("missing agents directory", func(t *testing.T) {
		missing := filepath.Join(tr.home, "nodir")
		_, err := agentctx.LoadFiles(tr.request(config.Context{Agents: []string{missing}}))
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
		_, err := agentctx.LoadFiles(tr.request(config.Context{DefaultAgents: true}))
		var pe *fs.PathError
		require.ErrorAs(t, err, &pe)
		require.Equal(t, path, pe.Path)
		require.ErrorIs(t, err, fs.ErrPermission)
	})
	t.Run("dangling AGENTS.md symlink", func(t *testing.T) {
		link := filepath.Join(tr.workDir, "AGENTS.md")
		require.NoError(t, os.Symlink("gone.md", link))
		t.Cleanup(func() { require.NoError(t, os.Remove(link)) })
		_, err := agentctx.LoadFiles(tr.request(config.Context{DefaultAgents: true}))
		var pe *fs.PathError
		require.ErrorAs(t, err, &pe)
		require.Equal(t, link, pe.Path)
	})
	t.Run("tilde without a home directory", func(t *testing.T) {
		req := tr.request(config.Context{Agents: []string{"~/notes.md"}})
		req.HomeDir = ""
		_, err := agentctx.LoadFiles(req)
		require.Error(t, err)
	})
}

func TestSizeLimitsAreErrorsNotTruncation(t *testing.T) {
	tr := newTree(t)
	root := write(t, filepath.Join(tr.repo, "AGENTS.md"), "0123456789")
	leaf := write(t, filepath.Join(tr.workDir, "AGENTS.md"), "0123456789")

	_, err := agentctx.LoadFiles(tr.request(config.Context{DefaultAgents: true, MaxFileBytes: 9}))
	var limit *agentctx.LimitError
	require.ErrorAs(t, err, &limit)
	require.Equal(t, agentctx.LimitError{Path: root, Key: "context.max_file_bytes", Size: 10, Limit: 9}, *limit)

	_, err = agentctx.LoadFiles(tr.request(config.Context{DefaultAgents: true, MaxFileBytes: 10, MaxTotalBytes: 19}))
	require.ErrorAs(t, err, &limit)
	require.Equal(t, agentctx.LimitError{Path: leaf, Key: "context.max_total_bytes", Size: 20, Limit: 19}, *limit)

	files, err := agentctx.LoadFiles(tr.request(config.Context{DefaultAgents: true, MaxFileBytes: 10, MaxTotalBytes: 20}))
	require.NoError(t, err)
	require.Len(t, files, 2)
}

func skillFile(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body
}

func TestSkillsAreListedAndLoadedOnDemand(t *testing.T) {
	tr := newTree(t)
	first, second := filepath.Join(tr.home, ".agents", "skills"), filepath.Join(tr.repo, "skills")
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

	// The default directory comes before the configured ones.
	cfg := config.Context{DefaultSkills: true, SkillsDirs: []string{second}}
	skills, err := agentctx.ListSkills(tr.request(cfg))
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
	loaded, err := agentctx.Load(tr.request(config.Context{
		DefaultAgents: true,
		SkillsDirs:    []string{"~/skills"},
		Sections: []config.ContextSection{
			{Name: "team", Text: "Team notes."},
			{Name: "house", File: "~/house.md"},
		},
	}))
	require.NoError(t, err)
	return tr, agentctx.Prompt{
		Instructions:   "UPSTREAM",
		Tools:          "TOOLS",
		ToolLines:      []string{"Use read.", "Use bash."},
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
		agentctx.SectionPrompt, agentctx.SectionTools, agentctx.SectionProjectContext,
		agentctx.SectionSkills, agentctx.SectionWorkDir, "team", "house",
	}, names(blocks))
	// The upstream text is passed through untouched.
	require.Equal(t, "UPSTREAM", blocks[0].Text)
	// The lines of the offered tools follow the general guidance.
	require.Equal(t, "TOOLS\n- Use read.\n- Use bash.", blocks[1].Text)
	require.Contains(t, blocks[2].Text, filepath.Join(tr.repo, "AGENTS.md"))
	require.Contains(t, blocks[2].Text, "Use tabs.")
	require.Contains(t, blocks[3].Text, "deploy: Ship a release")
	require.Contains(t, blocks[4].Text, tr.workDir)
	require.Contains(t, blocks[4].Text, tr.home)
	require.Equal(t, "House rules.", blocks[6].Text)

	// A section with nothing in it is left out, not sent empty.
	blocks, err = agentctx.Build(agentctx.Prompt{Instructions: "UPSTREAM", WorkDir: tr.workDir})
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
		require.Equal(t, base[2:], blocks[1:])
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
	loaded, err := agentctx.Load(tr.request(config.Context{
		Sections: []config.ContextSection{{Name: agentctx.SectionTools, Text: "mine"}},
	}))
	require.NoError(t, err)
	_, err = agentctx.Build(agentctx.Prompt{Instructions: "UPSTREAM", Loaded: loaded})
	require.Error(t, err)
}

// The default skills directory is absent on most machines, so a missing one
// holds no skills. A configured one was named on purpose and must exist.
func TestOnlyAConfiguredSkillsDirectoryMustExist(t *testing.T) {
	tr := newTree(t)
	skills, err := agentctx.ListSkills(tr.request(config.Context{DefaultSkills: true}))
	require.NoError(t, err)
	require.Empty(t, skills)

	missing := filepath.Join(tr.home, "no-such-dir")
	_, err = agentctx.ListSkills(tr.request(config.Context{SkillsDirs: []string{missing}}))
	var pe *fs.PathError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, missing, pe.Path)
}

func TestSystemPromptFileReplacesTheInstructions(t *testing.T) {
	tr := newTree(t)
	loaded, err := agentctx.Load(tr.request(config.Context{}))
	require.NoError(t, err)
	require.Equal(t, agentctx.Instructions, loaded.Instructions)

	write(t, filepath.Join(tr.home, "prompt.md"), "Be a reviewer.")
	loaded, err = agentctx.Load(tr.request(config.Context{SystemPromptFile: "~/prompt.md"}))
	require.NoError(t, err)
	require.Equal(t, "Be a reviewer.", loaded.Instructions)

	write(t, filepath.Join(tr.home, "blank.md"), " \n")
	_, err = agentctx.Load(tr.request(config.Context{SystemPromptFile: "~/blank.md"}))
	require.Error(t, err)
}

func TestASkillCacheSeesFilesChangeOnDisk(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	req := agentctx.Request{Config: config.Context{SkillsDirs: []string{dir}}, WorkDir: dir, Skills: &agentctx.SkillCache{}}
	list := func() []agentctx.Skill {
		t.Helper()
		skills, err := agentctx.ListSkills(req)
		require.NoError(t, err)
		return skills
	}
	deploy := write(t, filepath.Join(dir, "deploy", "SKILL.md"), skillFile("deploy", "Ship a release", "body"))
	audit := write(t, filepath.Join(dir, "audit", "SKILL.md"), skillFile("audit", "Audit tests", "body"))
	want := []agentctx.Skill{
		{Name: "audit", Description: "Audit tests", Dir: filepath.Dir(audit), Path: audit},
		{Name: "deploy", Description: "Ship a release", Dir: filepath.Dir(deploy), Path: deploy},
	}
	require.Equal(t, want, list())
	require.Equal(t, want, list())

	// An edit is seen by the next listing.
	write(t, deploy, skillFile("deploy", "Ship a release to production", "body"))
	want[1].Description = "Ship a release to production"
	require.Equal(t, want, list())

	// So is an edit that keeps the size, by its modification time.
	info, err := os.Stat(deploy)
	require.NoError(t, err)
	write(t, deploy, skillFile("deploy", "Ship a release to PRODUCTION", "body"))
	require.NoError(t, os.Chtimes(deploy, time.Time{}, info.ModTime().Add(time.Second)))
	want[1].Description = "Ship a release to PRODUCTION"
	require.Equal(t, want, list())

	// A file that breaks is an error each time, and is listed again once
	// it is mended.
	write(t, audit, "# no front matter\n")
	for range 2 {
		_, err = agentctx.ListSkills(req)
		require.ErrorContains(t, err, audit)
	}
	require.NoError(t, os.RemoveAll(filepath.Dir(audit)))
	require.Equal(t, want[1:], list())
}
