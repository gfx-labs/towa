package config

import "time"

// Tools configures the tools a model can call.
type Tools struct {
	// MaxOutputBytes is the most output one shell or search call keeps in
	// memory. What is over it is dropped and the result says how much.
	MaxOutputBytes int64       `koanf:"max_output_bytes" validate:"gt=0"`
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
	ReadMaxBytes int64 `koanf:"read_max_bytes" validate:"gt=0"`
	// ReadDefaultLines is how many lines a read returns when the call names
	// no limit.
	ReadDefaultLines int `koanf:"read_default_lines" validate:"gt=0"`
	// ImageMaxBytes is the largest image a read returns.
	ImageMaxBytes int64 `koanf:"image_max_bytes" validate:"gt=0"`
	// EditMaxBytes is the largest file an edit or an overwrite accepts. The
	// old and the new content are both held in memory for the diff.
	EditMaxBytes int64 `koanf:"edit_max_bytes" validate:"gt=0"`
}

// ToolsShell configures the shell tool.
type ToolsShell struct {
	// Program runs a command given as text: `program -c command`.
	Program string `koanf:"program" validate:"required"`
	// Timeout applies to a call that names none.
	Timeout time.Duration `koanf:"timeout" validate:"gt=0,ltefield=MaxTimeout"`
	// MaxTimeout is the longest timeout a call may ask for.
	MaxTimeout time.Duration `koanf:"max_timeout" validate:"gt=0"`
	// PipeWait is how long output is still read after the command has
	// exited or been killed, while a process it left behind holds the pipe.
	PipeWait time.Duration `koanf:"pipe_wait" validate:"gt=0"`
}

// ToolsSearch configures search.
type ToolsSearch struct {
	// Ripgrep is the rg binary, a path or a name looked up on PATH.
	Ripgrep string `koanf:"ripgrep" validate:"required"`
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
	BudgetTokens int64 `koanf:"budget_tokens" validate:"gt=0"`
	// BytesPerToken is the estimate: a result's text bytes divided by it.
	BytesPerToken int64 `koanf:"bytes_per_token" validate:"gt=0"`
}

// ToolsBatch configures the batch tool.
type ToolsBatch struct {
	// Enabled exposes the batch extra to the model.
	Enabled bool `koanf:"enabled"`
	// MaxCalls is the most calls one batch may hold. They may all run at
	// once.
	MaxCalls int `koanf:"max_calls" validate:"gt=0"`
}

// toolsDefaults names where each default comes from. A Codex figure cites
// its file and line in openai/codex at ce0323f2f2d3b25e82806cd555237da3db4e1897,
// read on 2026-11-11. A Claude Code figure cites a module of the unpacked
// 2.1.280 bundle, its line and the text to search for, since the bundle is
// minified and its identifiers change with every version. The jcode figure
// is unchecked. See docs/notes/2026-11-11-tools.md for what differs.
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
	// Claude Code: 262144 (chunk-esax8p5m.js:11, "=262144"), the default
	// maxSizeBytes of a read (chunk-fg23k495.js:11, "defaultFileReadingLimits").
	// Upstream applies it to the whole file and only when the call names no
	// limit. Here it is the most any one read returns.
	"tools.files.read_max_bytes": 256 << 10,
	// Claude Code: 2000 (chunk-58pe6gjn.js:12, "Reads up to").
	"tools.files.read_default_lines": 2000,
	// Claude Code: targetRawSize, 3932160 (chunk-6bcrsx06.js:11,
	// "maxBase64Size:5242880,targetRawSize:3932160"). The API's limit is
	// 5 MiB of base64, which is three quarters of that in file bytes.
	// Upstream shrinks a larger image. Here it is refused.
	"tools.files.image_max_bytes": 3932160,
	// No upstream source. Sixteen maximal reads, so a file the model can
	// page through in a few calls can be edited. Claude Code's Edit takes
	// a file of up to 1 GiB (chunk-1xqpf2j8.js:3451, "too large to edit").
	"tools.files.edit_max_bytes": 4 << 20,
	// No upstream source for a fixed program. Codex runs the user's own
	// shell and falls back to bash, then zsh, then /bin/sh on Linux
	// (codex-rs/shell-command/src/shell_detect.rs:363). Claude Code takes
	// CLAUDE_CODE_SHELL, then SHELL when it is bash or zsh, then looks for
	// both (chunk-1xqpf2j8.js:675, "Using shell override").
	"tools.shell.program": "bash",
	// Claude Code: 120000 and 600000 ms (chunk-dk0eh9zy.js:14,
	// "BASH_DEFAULT_TIMEOUT_MS"). Codex's default is ten seconds,
	// DEFAULT_EXEC_COMMAND_TIMEOUT_MS
	// (codex-rs/core/src/exec.rs:63), and it has no maximum there.
	"tools.shell.timeout":     "2m",
	"tools.shell.max_timeout": "10m",
	// Codex: IO_DRAIN_TIMEOUT_MS, 2000 (codex-rs/core/src/exec.rs:94).
	"tools.shell.pipe_wait": "2s",
	"tools.search.ripgrep":  "rg",
	"tools.repair.enabled":  true,
	"tools.gate.enabled":    true,
	// Claude Code: 25000, its limit on an MCP tool's result
	// (chunk-8ren4cq6.js:11, "MAX_MCP_OUTPUT_TOKENS") and on one file
	// read (chunk-fg23k495.js:11, "=25000"). Codex's nearest figure is lower:
	// DEFAULT_MAX_OUTPUT_TOKENS, 10000, for one command's output
	// (codex-rs/core/src/unified_exec/mod.rs:74).
	"tools.gate.budget_tokens": 25000,
	// Codex: APPROX_BYTES_PER_TOKEN, 4, rounded up as here
	// (codex-rs/utils/string/src/truncate.rs:6 and :96).
	"tools.gate.bytes_per_token": 4,
	// Unchecked: jcode's batch takes one to ten calls.
	"tools.batch.enabled":   true,
	"tools.batch.max_calls": 10,
}

func (t Tools) validate() error {
	return validateFields("tools", t)
}
