package config

import "time"

// Tools configures the tools a model can call.
//
// Each default names where it comes from. A Codex figure cites its file and
// line in openai/codex at ce0323f2f2d3b25e82806cd555237da3db4e1897, read on
// 2026-11-11. A Claude Code figure cites a module of the unpacked 2.1.280
// bundle, its line and the text to search for, since the bundle is minified
// and its identifiers change with every version. The jcode figure is
// unchecked. See docs/notes/2026-11-11-tools.md for what differs.
type Tools struct {
	// MaxOutputBytes is the most output one shell call keeps in memory.
	// What is over it is dropped and the result says how much.
	// Codex keeps the same amount: UNIFIED_EXEC_OUTPUT_MAX_BYTES, 1 MiB
	// (codex-rs/core/src/unified_exec/mod.rs:75), which exec.rs:81 uses
	// through DEFAULT_OUTPUT_BYTES_CAP (codex-rs/utils/pty/src/lib.rs:25).
	// It also keeps half from the start and half from the end
	// (codex-rs/core/src/unified_exec/head_tail_buffer.rs:18).
	MaxOutputBytes int64       `name:"max_output_bytes" default:"1048576" validate:"gt=0" help:"Most output one shell call keeps. The rest is dropped and the result says how much."`
	Files          ToolsFiles  `embed:"" prefix:"files."`
	Shell          ToolsShell  `embed:"" prefix:"shell."`
	Repair         ToolsRepair `embed:"" prefix:"repair."`
	Gate           ToolsGate   `embed:"" prefix:"gate."`
	Batch          ToolsBatch  `embed:"" prefix:"batch."`
	Todo           ToolsTodo   `embed:"" prefix:"todo."`
}

// ToolsFiles configures read, write and edit.
type ToolsFiles struct {
	// Confine refuses a path outside the session's working directory and
	// additional directories, for read, write, edit, the directory a
	// command runs in and the path a search covers. It does not confine
	// what a command does once it runs. It is off, as upstream: the Read
	// tool's description tells the model it can read any file on the
	// machine, and the design leaves isolation to the host.
	Confine bool `name:"confine" help:"Refuse a path outside the session's working directory and additional directories. It does not confine what a command does."`
	// ReadMaxBytes is the most one read returns. A larger range is an error
	// that tells the caller to ask for fewer lines. Claude Code: 262144
	// (chunk-esax8p5m.js:11, "=262144"), the default maxSizeBytes of a read
	// (chunk-fg23k495.js:11, "defaultFileReadingLimits"). Upstream applies
	// it to the whole file and only when the call names no limit. Here it
	// is the most any one read returns.
	ReadMaxBytes int64 `name:"read_max_bytes" default:"262144" validate:"gt=0" help:"Most bytes one read returns. A larger range is an error."`
	// ReadDefaultLines is how many lines a read returns when the call names
	// no limit. Claude Code: 2000 (chunk-58pe6gjn.js:12, "Reads up to").
	ReadDefaultLines int `name:"read_default_lines" default:"2000" validate:"gt=0" help:"Lines a read returns when the call names no limit."`
	// ImageMaxBytes is the largest image a read returns. Claude Code:
	// targetRawSize, 3932160 (chunk-6bcrsx06.js:11,
	// "maxBase64Size:5242880,targetRawSize:3932160"). The API's limit is
	// 5 MiB of base64, which is three quarters of that in file bytes.
	// Upstream shrinks a larger image. Here it is refused.
	ImageMaxBytes int64 `name:"image_max_bytes" default:"3932160" validate:"gt=0" help:"Largest image a read returns. A larger one is refused."`
	// EditMaxBytes is the largest file an edit or an overwrite accepts. The
	// old and the new content are both held in memory for the diff. No
	// upstream source. Sixteen maximal reads, so a file the model can page
	// through in a few calls can be edited. Claude Code's Edit takes a file
	// of up to 1 GiB (chunk-1xqpf2j8.js:3451, "too large to edit").
	EditMaxBytes int64 `name:"edit_max_bytes" default:"4194304" validate:"gt=0" help:"Largest file an edit or an overwrite accepts."`
}

// ToolsShell configures the shell tool.
type ToolsShell struct {
	// Program runs a command given as text: `program -c command`. No
	// upstream source for a fixed program. Codex runs the user's own shell
	// and falls back to bash, then zsh, then /bin/sh on Linux
	// (codex-rs/shell-command/src/shell_detect.rs:363). Claude Code takes
	// CLAUDE_CODE_SHELL, then SHELL when it is bash or zsh, then looks for
	// both (chunk-1xqpf2j8.js:675, "Using shell override").
	Program string `name:"program" default:"bash" validate:"required" help:"Program that runs a command given as text, as program -c command."`
	// Timeout applies to a call that names none. MaxTimeout is the longest
	// timeout a call may ask for. Claude Code: 120000 and 600000 ms
	// (chunk-dk0eh9zy.js:14, "BASH_DEFAULT_TIMEOUT_MS"). Codex's default is
	// ten seconds, DEFAULT_EXEC_COMMAND_TIMEOUT_MS
	// (codex-rs/core/src/exec.rs:63), and it has no maximum there.
	Timeout    time.Duration `name:"timeout" default:"2m" validate:"gt=0,ltefield=MaxTimeout" help:"Timeout of a command whose call names none."`
	MaxTimeout time.Duration `name:"max_timeout" default:"10m" validate:"gt=0" help:"Longest timeout a call may ask for."`
	// PipeWait is how long output is still read after the command has
	// exited or been killed, while a process it left behind holds the pipe.
	// Codex: IO_DRAIN_TIMEOUT_MS, 2000 (codex-rs/core/src/exec.rs:94).
	PipeWait time.Duration `name:"pipe_wait" default:"2s" validate:"gt=0" help:"How long output is still read after a command ended, while a process it left behind holds the pipe."`
}

// ToolsRepair configures argument repair.
type ToolsRepair struct {
	// Enabled turns the repairs on. Validation happens either way.
	Enabled bool `name:"enabled" default:"true" help:"Repair a tool call's arguments in the listed ways before they are validated. On by default."`
}

// ToolsGate configures output gating.
type ToolsGate struct {
	Enabled bool `name:"enabled" default:"true" help:"Hold back a tool result over the budget until the model asks for all of it. On by default."`
	// BudgetTokens is the largest result, in estimated tokens, that goes to
	// the model without it asking for the full output. Claude Code: 25000,
	// its limit on an MCP tool's result (chunk-8ren4cq6.js:11,
	// "MAX_MCP_OUTPUT_TOKENS") and on one file read (chunk-fg23k495.js:11,
	// "=25000"). Codex's nearest figure is lower:
	// DEFAULT_MAX_OUTPUT_TOKENS, 10000, for one command's output
	// (codex-rs/core/src/unified_exec/mod.rs:74).
	BudgetTokens int64 `name:"budget_tokens" default:"25000" validate:"gt=0" help:"Largest result, in estimated tokens, that goes to the model without it asking for the full output."`
	// BytesPerToken is the estimate: a result's text bytes divided by it.
	// Codex: APPROX_BYTES_PER_TOKEN, 4, rounded up as here
	// (codex-rs/utils/string/src/truncate.rs:6 and :96).
	BytesPerToken int64 `name:"bytes_per_token" default:"4" validate:"gt=0" help:"Bytes of a result's text counted as one token."`
}

// ToolsBatch configures the batch tool.
type ToolsBatch struct {
	// Enabled exposes the batch extra to the model.
	Enabled bool `name:"enabled" default:"true" help:"Give the model the batch tool. On by default."`
	// MaxCalls is the most calls one batch may hold. They may all run at
	// once. Unchecked: jcode's batch takes one to ten calls.
	MaxCalls int `name:"max_calls" default:"10" validate:"gt=0" help:"Most calls one batch may hold."`
}

// ToolsTodo configures the todo tool.
type ToolsTodo struct {
	// Enabled gives the model the todo tool. It is off by default: the
	// tool's definition is about 1,100 tokens of every request
	// (docs/notes/2026-11-12-token-use.md).
	Enabled bool `name:"enabled" help:"Give the model the todo tool, whose list the client shows as the plan."`
}

func (t Tools) validate() error {
	return validateFields("tools", t)
}
