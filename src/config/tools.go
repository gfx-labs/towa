package config

import (
	"errors"
	"fmt"
	"time"
)

// Tools configures the tools a model can call.
type Tools struct {
	// MaxOutputBytes is the most output one shell or search call keeps in
	// memory. What is over it is dropped and the result says how much.
	MaxOutputBytes int64       `koanf:"max_output_bytes"`
	Files          ToolsFiles  `koanf:"files"`
	Shell          ToolsShell  `koanf:"shell"`
	Search         ToolsSearch `koanf:"search"`
	Repair         ToolsRepair `koanf:"repair"`
	Gate           ToolsGate   `koanf:"gate"`
	Batch          ToolsBatch  `koanf:"batch"`
}

// ToolsFiles configures read, write and edit.
type ToolsFiles struct {
	// Confine refuses a path outside the session's working directory and
	// additional directories, for read, write, edit, the directory a
	// command runs in and the path a search covers. It does not confine
	// what a command does once it runs.
	Confine bool `koanf:"confine"`
	// ReadMaxBytes is the most one read returns. A larger range is an error
	// that tells the caller to ask for fewer lines.
	ReadMaxBytes int64 `koanf:"read_max_bytes"`
	// ReadDefaultLines is how many lines a read returns when the call names
	// no limit.
	ReadDefaultLines int `koanf:"read_default_lines"`
	// ImageMaxBytes is the largest image a read returns.
	ImageMaxBytes int64 `koanf:"image_max_bytes"`
	// EditMaxBytes is the largest file an edit or an overwrite accepts. The
	// old and the new content are both held in memory for the diff.
	EditMaxBytes int64 `koanf:"edit_max_bytes"`
}

// ToolsShell configures the shell tool.
type ToolsShell struct {
	// Program runs a command given as text: `program -c command`.
	Program string `koanf:"program"`
	// Timeout applies to a call that names none.
	Timeout time.Duration `koanf:"timeout"`
	// MaxTimeout is the longest timeout a call may ask for.
	MaxTimeout time.Duration `koanf:"max_timeout"`
	// PipeWait is how long output is still read after the command has
	// exited or been killed, while a process it left behind holds the pipe.
	PipeWait time.Duration `koanf:"pipe_wait"`
}

// ToolsSearch configures search.
type ToolsSearch struct {
	// Ripgrep is the rg binary, a path or a name looked up on PATH.
	Ripgrep string `koanf:"ripgrep"`
}

// ToolsRepair configures argument repair.
type ToolsRepair struct {
	// Enabled turns the repairs on. Validation happens either way.
	Enabled bool `koanf:"enabled"`
}

// ToolsGate configures output gating.
type ToolsGate struct {
	Enabled bool `koanf:"enabled"`
	// BudgetTokens is the largest result, in estimated tokens, that goes to
	// the model without it asking for the full output.
	BudgetTokens int64 `koanf:"budget_tokens"`
	// BytesPerToken is the estimate: a result's text bytes divided by it.
	BytesPerToken int64 `koanf:"bytes_per_token"`
}

// ToolsBatch configures the batch tool.
type ToolsBatch struct {
	// MaxCalls is the most calls one batch may hold. They may all run at
	// once.
	MaxCalls int `koanf:"max_calls"`
}

// toolsDefaults names where each default comes from. A Codex figure cites
// its file and line in openai/codex at ce0323f2f2d3b25e82806cd555237da3db4e1897,
// read on 2026-11-11. The Claude Code and jcode figures were written from
// memory and are unchecked until the capture lane's files are in: see
// docs/notes/2026-11-11-tools.md.
var toolsDefaults = map[string]any{
	// Codex keeps the same amount: UNIFIED_EXEC_OUTPUT_MAX_BYTES, 1 MiB
	// (codex-rs/core/src/unified_exec/mod.rs:75), which exec.rs:81 uses
	// through DEFAULT_OUTPUT_BYTES_CAP (codex-rs/utils/pty/src/lib.rs:25).
	// It also keeps half from the start and half from the end
	// (codex-rs/core/src/unified_exec/head_tail_buffer.rs:18).
	"tools.max_output_bytes": 1 << 20,
	// Off, as upstream: the Read tool's description tells the model it can
	// read any file on the machine, and the design leaves isolation to the
	// host.
	"tools.files.confine": false,
	// Unchecked: Claude Code's Read, 256 KB per read and 2000 lines.
	"tools.files.read_max_bytes":     256 << 10,
	"tools.files.read_default_lines": 2000,
	// Unchecked: the Anthropic Messages API's limit on one image, 5 MB.
	"tools.files.image_max_bytes": 5 << 20,
	// No upstream source. Sixteen maximal reads, so a file the model can
	// page through in a few calls can be edited.
	"tools.files.edit_max_bytes": 4 << 20,
	// No upstream source for a fixed program. Codex runs the user's own
	// shell and falls back to bash, then zsh, then /bin/sh on Linux
	// (codex-rs/shell-command/src/shell_detect.rs:363). Unchecked: Claude
	// Code's Bash tool.
	"tools.shell.program": "bash",
	// Unchecked: Claude Code's Bash, two minutes by default and ten at
	// most. Codex's default is ten seconds, DEFAULT_EXEC_COMMAND_TIMEOUT_MS
	// (codex-rs/core/src/exec.rs:63), and it has no maximum there.
	"tools.shell.timeout":     "2m",
	"tools.shell.max_timeout": "10m",
	// Codex: IO_DRAIN_TIMEOUT_MS, 2000 (codex-rs/core/src/exec.rs:94).
	"tools.shell.pipe_wait": "2s",
	"tools.search.ripgrep":  "rg",
	"tools.repair.enabled":  true,
	"tools.gate.enabled":    true,
	// Unchecked: Claude Code's limit on a tool result,
	// MAX_MCP_OUTPUT_TOKENS. Codex's nearest figure is lower:
	// DEFAULT_MAX_OUTPUT_TOKENS, 10000, for one command's output
	// (codex-rs/core/src/unified_exec/mod.rs:74).
	"tools.gate.budget_tokens": 25000,
	// Codex: APPROX_BYTES_PER_TOKEN, 4, rounded up as here
	// (codex-rs/utils/string/src/truncate.rs:6 and :96).
	"tools.gate.bytes_per_token": 4,
	// Unchecked: jcode's batch takes one to ten calls.
	"tools.batch.max_calls": 10,
}

func (t Tools) validate() error {
	positive := []struct {
		key   string
		value int64
	}{
		{"tools.max_output_bytes", t.MaxOutputBytes},
		{"tools.files.read_max_bytes", t.Files.ReadMaxBytes},
		{"tools.files.read_default_lines", int64(t.Files.ReadDefaultLines)},
		{"tools.files.image_max_bytes", t.Files.ImageMaxBytes},
		{"tools.files.edit_max_bytes", t.Files.EditMaxBytes},
		{"tools.shell.timeout", int64(t.Shell.Timeout)},
		{"tools.shell.max_timeout", int64(t.Shell.MaxTimeout)},
		{"tools.shell.pipe_wait", int64(t.Shell.PipeWait)},
		{"tools.gate.budget_tokens", t.Gate.BudgetTokens},
		{"tools.gate.bytes_per_token", t.Gate.BytesPerToken},
		{"tools.batch.max_calls", int64(t.Batch.MaxCalls)},
	}
	for _, p := range positive {
		if p.value <= 0 {
			return fmt.Errorf("%s: must be greater than zero", p.key)
		}
	}
	if t.Shell.Timeout > t.Shell.MaxTimeout {
		return fmt.Errorf("tools.shell.timeout: %s is over tools.shell.max_timeout, %s", t.Shell.Timeout, t.Shell.MaxTimeout)
	}
	if t.Shell.Program == "" {
		return errors.New("tools.shell.program: must not be empty")
	}
	if t.Search.Ripgrep == "" {
		return errors.New("tools.search.ripgrep: must not be empty")
	}
	return nil
}
