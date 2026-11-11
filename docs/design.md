# acpharness design

acpharness is a Go coding agent that speaks the Agent Client Protocol (ACP). It runs Claude and Codex models with their upstream tool shapes over the user's subscriptions, other models over OpenAI-compatible APIs, and adds a small set of jcode harness features. It targets hundreds of concurrent sessions per Linux host, run directly or behind acphost.

Status: draft, 2026-11-11. Decisions come from four Q&A rounds with the user.

## Goals

- ACP compliant agent: `initialize`, `authenticate`, `session/new`, `session/load`, `session/prompt`, `session/cancel`, `session/set_config_option` (model, mode, thought level), `session/set_mode`, `session/request_permission`, `session/update` with plans, tool calls, thoughts, commands and usage. Method names follow the pinned ACP schema, which has no `session/set_model`.
- Does real coding tasks locally as well as jcode does in a single session.
- Low footprint per session: idle sessions use no CPU and little memory.
- Claude OAuth, Codex OAuth, OpenAI Chat Completions and Responses, OpenRouter.
- Upstream tool shapes and system prompts per model family, pinned by version.

## Non-goals (v1)

- TUI, sandboxing or isolation (wrappers such as acphost do that), swarm (leave room), self-learning or memory.
- macOS as a tested platform. It is a headless build target only: the tree must cross-build for `darwin/arm64`, because iOS captures run on a Mac.

## First milestone

The agent completes coding tasks locally with quality comparable to jcode and passes ACP compliance checks with real ACP clients (acphost, and jobshop's `acp-go-sdk` v0.13.5 client). jobcity integration and footprint comparison come after.

## Architecture

```mermaid
flowchart LR
  client[ACP client: acphost, jobshop, editor] -->|stdio / unix / tcp+mTLS| acp[acp server]
  acp --> sess[session manager]
  sess --> loop[agent loop]
  loop --> prov[provider router]
  prov --> anth[anthropic oauth/api]
  prov --> oai[openai responses / codex oauth]
  prov --> cc[chat completions / openrouter]
  loop --> tools[tool registry]
  tools --> dialect[dialects: claude, codex, generic]
  tools --> extras[extras: batch, gate, search, todo, web, subagent]
  tools --> mcp[mcp meta-tool]
  loop --> compact[compaction]
  sess --> store[(session store)]
  prov --> auth[(auth store)]
```

Packages:

| package | role |
|---|---|
| `cmd/acpharness` | a shim that calls `src/cli` |
| `src/cli` | CLI: `serve` (default, stdio), `login`, `logout`, `accounts`, `config`, `models`, `hosts` |
| `pkg/acp/harness` | what acpharness adds to ACP messages under `_meta.acpharness`, as types a client imports |
| `src/transcript` | the one conversation format shared by providers, tools and the store |
| `src/httpclient` | the one constructor for HTTP clients: proxy from the environment, system certificate pool |
| `src/config` | koanf loading and the typed config |
| `pkg/acp` | our ACP layer: JSON-RPC connection, types generated from the pinned schema, transports, update mapping |
| `src/session` | session lifecycle, modes, model switching, cancel |
| `src/loop` | one prompt turn: stream, tool dispatch, limits, hooks |
| `src/provider` | provider interface, router, fallback, retry |
| `src/provider/{anthropic,openai,chat}` | wire clients |
| `src/auth` | account store, OAuth login and refresh, file locking |
| `src/tools` | registry, dialect selection, output gating |
| `src/tools/{claude,codex,generic,extras}` | tool sets |
| `src/mcp` | MCP client: stdio, streamable HTTP, legacy SSE, OAuth |
| `src/compact` | reactive, proactive, semantic, OpenAI native |
| `src/agentctx` | AGENTS.md, skills, extra files and directories |
| `src/store` | store interface and backends |
| `src/telemetry` | OTel traces and metrics, slog handlers |
| `prompts/` | pinned upstream prompts and tool schemas, embedded |

`src/` holds packages while APIs settle. Packages move to `pkg/` when a real external caller needs them. `pkg/acp` moved on 2026-11-11: protocol7's worker imports it as the client side, so both ends share schema 1.31.0.

## Process and transports

- One process can host many sessions. The default is one process per stdio connection, which is what acphost spawns.
- Listeners: stdio (default), unix socket, TCP. TCP and unix support TLS with optional client certificate verification (mTLS) from config.
- A listener process serves many connections, each with its own sessions. Sessions in one process share the provider HTTP pool, auth store, MCP connections where config marks a server shared, and the prompt and schema caches.
- Idle cost: no goroutine wakes while a session waits for a prompt. No polling timers. Timers start only for active turns, retries and refreshes.

## Configuration

koanf, layered in this order, each later layer overriding earlier:

1. Built-in defaults.
2. `/etc/acpharness/config.{toml,yaml,json}`.
3. `$XDG_CONFIG_HOME/acpharness/config.*`.
4. Files listed in `ACPHARNESS_CONFIG` (path list, `:`-separated), and `--config` flags (repeatable).
5. Environment `ACPHARNESS_*`, `__` as the nesting separator (`ACPHARNESS_LIMITS__MAX_TOOL_CALLS=200`).
6. CLI flags, read from urfave/cli v3 by koanf's `cliflagv3` provider. A repeatable `--set key=value` reaches config keys without dedicated flags. Only explicitly supplied flags override earlier layers.
7. Per-session overrides from `session/new` `_meta.acpharness` (allowlisted keys only: model, context files, skills dirs, limits, mode, mcp servers, compaction).

koanf decodes the merged tree into one typed config struct. urfave/cli v3 defines commands and flags, and the application reads configuration only from the decoded struct. Flag names and environment names are mapped explicitly to the same canonical config keys. `go-playground/validator/v10` checks field constraints through `validate` tags after decoding. Configuration-specific rules, such as account references, endpoint policies and listener safety, remain explicit checks. Direct section validators use the same field constraints as the loader.

`acpharness config` prints the resolved config with secrets redacted and the source of each key.

Sketch. The examples in this document are YAML. A TOML or JSON file with the same keys loads the same way:

```yaml
default_model: opus

models:
  opus:
    chain:
      - "claude:work/claude-opus-4-7"
      - "claude:personal/claude-opus-4-7"
      - "openrouter:main/anthropic/claude-opus-4.7"
    # The last entry is a gateway's own id, which the built-in table does not
    # know, so the model states its figures.
    context_window: 1000000
    max_output_tokens: 128000
    family: claude
  gpt:
    chain:
      - "codex:chatgpt/gpt-5.5"

# claude:work, claude:personal and codex:chatgpt have no entry here: a login
# makes each of them in auth.json. Only an account without a stored login is
# declared.
accounts:
  - id: main
    provider: openrouter
    kind: api_key
    api_key_env: OPENROUTER_API_KEY
  - id: pod
    provider: claude
    kind: proxied_oauth   # oauth | api_key | proxied_oauth | proxied_api_key

providers:
  - id: openrouter
    kind: chat            # chat | responses
    base_url: https://openrouter.ai/api/v1
  - id: zai
    kind: chat
    base_url: https://api.z.ai/api/coding/paas/v4

context:
  agents_files: true
  extra_files: []
  extra_dirs: []
  skills_dirs: ["~/.agents/skills"]

limits:
  max_tool_calls: 0       # 0 = unlimited
  max_wall: 0s
  max_input_tokens: 0
  max_output_tokens: 0
  max_stop_denials: 3     # how often a stop hook may keep a turn going

subagents:
  max_depth: 2
  max_concurrent: 4

compaction:
  mode: reactive          # reactive | proactive | semantic | off
  openai_native: auto     # auto | explicit | off

hooks:
  - event: pre_tool       # pre_tool | post_tool | prompt | stop
    tools: [Bash, shell]
    command: [/usr/local/bin/check-cmd]
    timeout: 5s

mcp:
  servers:
    - name: linear
      transport: streamable_http   # stdio | streamable_http | sse
      url: https://mcp.linear.app/mcp

telemetry:
  otel: true
  logs:
    stderr:
      level: info
      format: text
    otel:
      level: debug
```

Defaults for limits and compaction thresholds start from jcode's measured defaults and are documented with their source.

## Providers and auth

### Provider interface

One streaming interface returning a normalized event stream: text deltas, thinking deltas (with signatures where the API has them), tool call starts, argument deltas, tool call ends, usage, stop reason, and provider-native items that must round-trip (Anthropic thinking signatures, OpenAI encrypted reasoning, compaction items). The stream is a Go iterator (`iter.Seq2[Event, error]`), as in `gfx-labs/sse`. A provider error is classified as one of three: retry the same chain entry, fall back to the next entry, or fail the turn.

Wire clients:

- `anthropic`: Messages API, streaming, OAuth or API key. OAuth identity follows jcode's code and its probe (`docs/ANTHROPIC_OAUTH_IDENTITY_PROBE_2026_11_05.md` in jcode). The one required piece is the `x-anthropic-billing-header` text sent as the first system block (`cc_version=2.1.280; cc_entrypoint=...; cch=33f85;`), which Anthropic strips server-side, with jcode's OAuth beta header set. Without that block, Opus and Sonnet OAuth requests get HTTP 429. jcode sends its own user agent (`jcode/<version>`) and no `x-app`, `X-Stainless-*` or Claude Code session headers, because the probe found none of them are needed. acpharness sends `acpharness/<version>`, and the prompt names the agent as acpharness. jcode's branches differ on `cc_entrypoint` (`sdk-cli` on origin/master 8e11eb23e, `jcode` on build 037163c7b), so step 3 re-runs the probe before choosing.
- `openai`: Responses API, streaming. Codex OAuth through `chatgpt.com/backend-api/codex` with the Codex instructions. jcode's 2026-11-05 probe found the backend accepts any `originator`, so acpharness sends `acpharness`. API keys against `api.openai.com` or any Responses-compatible endpoint. Native compaction.
- `chat`: Chat Completions, streaming, for OpenRouter, z.ai, DeepSeek, OpenCode Go and others. OpenRouter extras: provider routing preferences, reasoning parameters, `HTTP-Referer` and `X-OpenRouter-Title` (the header OpenRouter's app attribution page documents).

Model metadata (context window, max output, reasoning support, family, tool dialect) comes from a pinned table plus provider model listings (`/models` on OpenRouter), with config overrides. An unknown model with no metadata is an error at selection time, not a silent default.

### Accounts and fallback

- Logins live in one file, `$XDG_STATE_HOME/acpharness/auth/auth.json`, mode 0600, apart from the configuration. It holds every OAuth account by id with its provider and tokens. An account in it needs no `accounts` entry: a login creates it. A model chain may name an account of a built-in provider before its first login, and a turn that reaches it then reports that the account is not logged in and gives the command. `accounts` in the configuration is for what has no stored login: API keys read from the environment and proxy-held accounts. `logout` removes the account's entry.
- Proxy-held accounts (`proxied_oauth`, `proxied_api_key`): the harness never holds or refreshes the token. It sends a placeholder that names the account and builds the rest of the request exactly as for a token it held, including the Claude OAuth billing block and beta headers, because the proxy only swaps the authorization. Such an account has no entry in `auth.json`, `login` refuses it, and a rejected credential is reported as an auth stop, since there is nothing to refresh.
- Every HTTP client honours `HTTPS_PROXY` and the system certificate pool: providers, the OpenAI WebSocket transport, remote MCP, webfetch, embeddings, Jev and OTLP. They all come from `src/httpclient`, lint forbids building a transport anywhere else, and one CI test runs a turn through a test proxy with its own CA.
- `acpharness hosts` lists the hosts the resolved config will contact (providers, OAuth endpoints, MCP servers, embeddings, Jev, OTLP), one per line with the config key that names it. It builds an egress allowlist without guessing. webfetch targets are chosen by the model and cannot be listed.
- `acpharness login <provider> --account <id>` creates the account when it is new and logs it in. It prints the authorize page and opens a browser, which a local callback ends. A host without an opener still prints the page, and the login waits. `--manual` opens nothing and takes the code the page ends on. `--device` (Codex only, as upstream has no device grant for Claude) prints a one-time code and the page to enter it on, then waits for the approval, with the requests of `codex-rs/login/src/device_code_auth.rs` at the pinned commit.
- A browser or manual login is recorded under `auth/pending/<provider>.json` before its page is shown. A later run, `acpharness login <provider> --code '<code or address>'`, finishes it: the record names the account, so that run needs only the provider. There is one record per provider, a new login replaces it, and a finished login removes it. A manual login whose input is empty or closed leaves the record and exits, which is how a script or an agent starts a login that a person finishes.
- Refresh: take an `flock` on the account's lock file (`account.<id>.lock`, not `auth.json`, since the rename that writes the file would leave a lock on the old inode), re-read the account, refresh only if the token is still expired, write, release. Hundreds of processes share one refresh, and the refresh of one account does not wait for another's token request. Every write of `auth.json` takes `auth.json.lock` for its read and its write, so that two accounts written at once both survive, and replaces the file atomically (temp file and rename). A read takes no lock.
- Model chains: each named model is an ordered list of `provider:account/model`. The router tries the next entry on rate limit, quota exhaustion, auth failure or overload, after retries on the current entry are exhausted. Fallback moves are reported to the client.
- Retry: exponential backoff with jitter, honoring `retry-after` and provider reset headers. Policy in config.
- Rate-limit state is per account and shared in process. Cross-process sharing is a later option (a small file or broker) if contention shows up.

### Runtime model selection

- `session/new` uses `_meta` or the configured default.
- `session/set_config_option` with the `model` category switches between turns. The session advertises its named models as a select option. Plain messages and structural tool calls remain portable. Opaque replay requires an exact canonical provider, wire model and configured account match. Incompatible optional thinking is omitted; required signatures and native context fail explicitly or are regenerated from retained source before a request is sent.
- Subagent spawns may name a model or chain.

## Tools

### Dialects

| family | dialect | tools |
|---|---|---|
| Claude | `claude` | the tools captured from the pinned Claude Code version |
| GPT, Codex | `codex` | the tools captured from the pinned Codex version |
| other | resolved | the dialect of the family the model was distilled from when known, else `generic` |

`generic` uses captured Claude Code shapes with explicitly harness-authored neutral descriptions and system instructions. Config can pin a dialect per model. Generic instructions accept the exact configured wire model without aliasing it to an upstream model. Pinned upstream prompt lookup remains exact and refuses unavailable variants.

Each capability (read, write, edit, shell, search) has one implementation. A dialect adapter supplies the upstream tool name, schema, output text and error text. An adapter must behave as the upstream tool does, and is checked against outputs recorded from that upstream tool. Upstream prompts and schemas are copied into `prompts/<upstream>/<version>/` with the source commit, and a script diffs them against new upstream releases.

Upstream pins follow jcode: Claude Code 2.1.280, the version its billing block names, and openai/codex ce0323f2 (2026-10-17), the newest commit it cites for the OAuth contract. jcode sends its own tool names on the Claude OAuth path and carries no captured upstream tool schemas, so acpharness captures tool names, descriptions, schemas and system prompts from those two versions in steps 3 and 4. nanocode holds captures for Claude Code 2.1.304 and openai/codex c6e4438a with a verification script, usable as a cross-check.

### Extras (on by default, each can be disabled)

- Parallel execution of tool calls emitted in one message. Read-only tools run concurrently; writes to the same path serialize.
- `batch`: run several independent tool calls in one call, for models that do not emit parallel calls.
- Output gating: results over a configured token budget return a summary with the estimated size; re-running with `accept_large_output` returns the full result.
- Search: an agentgrep-style tool with grep, find, outline and trace modes. Text search requires the `rg` binary and reports a clear error when it is missing. Structural modes need a separately specified implementation, not a claimed ripgrep capability.
- Todo: maps to ACP `plan` updates. Uses TodoWrite or update_plan shapes in their dialects.
- Web: webfetch with HTML to Markdown. Web search uses the provider's built-in server tool (Anthropic web search, OpenAI Responses web search, OpenRouter web plugin). Users who want an external backend add a tool or an MCP server.
- Conversation search: a `conversation_search` tool with jcode's shape (`query`, `turns` range, `stats`). It reads the session's full stored history, including entries from before any compaction, not the compacted context the model currently sees. It can also read other sessions by id, so a parent can read the sessions of the subagents it spawned. Scope is a config setting: `self`, `descendants` (the session and every subagent session below it, the default) or `store` (every session in the store, opt-in because a shared store holds other workers' sessions). Keyword matching first. Result and snippet limits come from config.
- Argument repair: before validation, a tool input is repaired when a list or object argument arrives as a JSON string, a single object arrives where a list is declared, or a number or boolean arrives as a string. Each repair is counted in telemetry and logged, never silent. Anything else that fails the schema is returned to the model as an error.
- Subagent: see below.

Not in v1: background shell tasks.

### Subagents

The tool runs a child agent loop in the same process. Each child opens its own runtime with a read-authorization tracker, frozen tool inventory, MCP manager and compaction state. It inherits the parent's turn settings, including MCP server declarations, and may select another model or chain. The process shares provider clients, auth and child admission. Only configured MCP servers explicitly marked shared reuse physical connections; session overrides do not. The child runtime closes after its turn and tool dispatch have stopped, without closing its parent's resources. Its transcript is stored as its own session with the parent id, so the parent can search it. The parent receives the child's final message and reported usage, including usage reported before a child failure. Depth and concurrency limits come from config. Child activity streams as nested tool call updates. Swarm with separate sessions comes later.

### MCP

- Transports: stdio, streamable HTTP, legacy HTTP+SSE. OAuth for remote servers with tokens in the auth store.
- Servers come from config and from `session/new` `mcpServers`, merged by name with the session winning. A session's server replaces a configured server of the same name whole; no field is merged.
- One `mcp` meta-tool with `list`, `search` (names and descriptions) and `call`. Schemas are returned on search, not loaded into the prompt.
- Direct tools: a server marked `direct` has its tools put in the prompt under their own names, not behind the meta-tool. A server named in `session/new` `mcpServers` is direct by default, since the client added it for this session and its tools (`submit_result`, `ask_human`) are used every turn. A config server is behind the meta-tool by default. A direct tool whose name collides with another tool is an error at session start. The direct tools are fixed when the session starts: a server that changes its tool list later does not change the prompt.
- Connections start lazily on first use and close after an idle period from config.

### Permissions and modes

- Modes: `auto` (default, allow all, like jcode) and `ask` (routes `session/request_permission` for tools marked writing or executing). Exposed through ACP session modes.
- Slash commands through `available_commands`: `/compact`, `/model`, `/mode`, `/cost`.

### Hooks

Config maps an event and a tool set to a command. The command gets the event as JSON on stdin and returns JSON: allow, deny with reason, or modified input. Events: `pre_tool`, `post_tool`, `prompt`, `stop`. Timeouts come from config.

### Limits

Per-prompt limits from config or `_meta`: max tool calls, wall time, input and output tokens, subagent depth and concurrency. Hitting a limit ends the turn with a stop reason the client can see. Cancellation through `session/cancel` stops the stream, kills running tool processes by process group, and records the outcome of each in-flight tool as completed, cancelled or unknown.

### Interruption

- A `session/prompt` that arrives while a turn is running cancels that turn and starts the new one, as `acp-go-sdk` v0.13.5 does. The cancelled request returns the `cancelled` stop reason.
- After a cancel, every in-flight tool call gets a result saying it was cancelled, so no tool call is left without a result.
- The model is then told the turn was interrupted, with the dialect's upstream interruption message placed before the next user message. Codex: the `<turn_aborted>` message, which says the user interrupted on purpose and that aborted commands may have partly run. Claude Code: its request-interrupted message, to be confirmed against the pinned upstream version. Both texts are pinned with the other upstream prompts.

## Compaction

Ported from jcode:

- `reactive`: compact in the background when context reaches a fraction or a token count, with emergency hard compaction when a request would not fit.
- `proactive`: project growth with EWMA over a lookahead window, with floor, sample, stall and cooldown settings.
- `semantic`: topic-shift detection and relevance keeping with embeddings from a configured embedding provider (see Embeddings).
- `off`: stop the turn with a context-full reason.
- OpenAI native: `auto` (server-side compaction at a token threshold), `explicit` (call the compact endpoint at the threshold), `off`.
- `max_context_tokens` caps the budget below the model window.
- `/compact` triggers manually. Compaction is reported with `compaction_update` when the client advertises `session.compaction`, and as an ordinary agent message otherwise.
- Compaction appends an entry holding the summary and the id of the first kept entry. It never rewrites or deletes earlier entries, so conversation search can still read them.
- Two summary prompts, selected by `compaction.summary_prompt`: `checkpoint` (default, Pi's structured format with its merge prompt for later compactions) and `short` (jcode's). Either one tells the model it can search earlier history only when `conversation_search` is enabled.

## Embeddings

One client for the OpenAI `/v1/embeddings` shape, configured by base URL, model, account and dimensions. No in-process model. Local embeddings run as a sidecar that serves the same API: Ollama, llama.cpp `llama-server --embedding`, Hugging Face TEI, or Infinity. Hosted options use the same client: OpenAI, OpenRouter, Voyage, Jina, Mistral, Gemini through its OpenAI-compatible endpoint. Providers with a different wire shape get an adapter only when needed.

Recommended sidecar: Hugging Face Text Embeddings Inference (TEI). It runs any supported Hub model by id and serves OpenAI-compatible `/v1/embeddings`:

```sh
docker run -d -p 8080:80 -v $HOME/.cache/tei:/data \
  ghcr.io/huggingface/text-embeddings-inference:cpu-latest \
  --model-id BAAI/bge-small-en-v1.5
```

```yaml
embeddings:
  base_url: http://127.0.0.1:8080/v1
  model: BAAI/bge-small-en-v1.5
```

## Jev decisions

A configured Jev endpoint backs a native `jev_decision` tool and skill: bounded choice among supplied options, with state treated as untrusted data, matching jcode's tool shape and limits. Routes follow jcode: OpenRouter decisions, TypeSafe direct, AIMLAPI, or a custom base URL. Jev serves decisions only, not embeddings, so embeddings stay a separate config. Harness features may use Jev later (skill suggestion, subagent model routing) behind config.

```yaml
jev:
  route: openrouter     # openrouter | typesafe | aimlapi | custom
  model: typesafe/jev-1.13
  account: main
```

## System prompt

The prompt is built from named sections in a fixed order: the dialect's upstream prompt, tools, rules, project context, skills, working directory, and any sections added by config. ACP `systemPrompt` on `session/new` maps onto them: `append` adds a section, and `override` replaces the default instruction sections and leaves tools, MCP servers and permissions alone. An unsupported mode is an error. Appending a changed section mid-session to keep the prompt cache is deferred until a measurement shows sections change mid-session.

## Context loading

- AGENTS.md (and CLAUDE.md for Claude dialect) from the working directory up to the repository root, plus the global file.
- Skills: directories with `SKILL.md`, listed by name and description in the system prompt and loaded on demand by a `skill` tool.
- Extra files and directories from config or `_meta`.

## Sessions and storage

Store contract (`src/store/store.go`):

```go
type Store interface {
    Create(ctx context.Context, meta Meta) error
    Append(ctx context.Context, id uuid.UUID, entries []transcript.Entry) error
    Load(ctx context.Context, id uuid.UUID) (Meta, iter.Seq2[transcript.Entry, error], error)
    History(ctx context.Context, id uuid.UUID, r Range) (iter.Seq2[transcript.Entry, error], error)
    Repair(ctx context.Context, id uuid.UUID) (setAside int64, err error)
    Fork(ctx context.Context, source uuid.UUID, meta Meta) error
    SetMeta(ctx context.Context, meta Meta) error
    List(ctx context.Context, filter Filter) ([]Meta, error)
    Delete(ctx context.Context, id uuid.UUID) error
    PutBlob(ctx context.Context, id uuid.UUID, mimeType string, content io.Reader) (transcript.Stored, error)
    OpenBlob(ctx context.Context, id uuid.UUID, hash string) (io.ReadCloser, error)
}
```

- `transcript.Entry` is the append-only canonical transcript: messages, tool calls and results, provider-native items, model switches, compaction summaries. Provider-only content is in `Item.Opaque`, tagged with canonical `Item.Provider` and `Item.Origin` (exact request model and configured account ID). Opaque replay requires all three identities to match. A legacy nil origin is unknown and is never inferred or backfilled. Only safe plain structural fallback is allowed without a match, required signatures and native context need an explicit incompatibility error or portable regeneration from retained source.
- Session and entry ids are caller-supplied UUIDv7 from the Go standard library. Times also come from the caller. Each entry keeps its `ParentID`, including when a fork copies it.
- `Meta` records cwd, additional directories, title, model, mode, parent session id, created time and archived state. `State` is opaque caller-owned bytes for session configuration the load request cannot carry. The store returns those bytes unchanged and never interprets them. `SetMeta` replaces Title, Model, Mode, Dirs, Archived and State, leaving identity, cwd, parent and created time unchanged.
- `Append` returns only after the entries are durable, all together. In either format, a crash leaves a complete append or an explicitly torn one, never a silently accepted prefix. `Updated` is the time of the last entry, read from the log rather than rewriting metadata each turn.
- `Load` yields the latest `Compaction` entry and everything after it, without reading the history before it. There is no separate snapshot method. `History` selects entry positions from the full log, including entries before compaction.
- A torn log yields its complete entries followed by an error matching `store.ErrTorn`. `Append` refuses the same log. The session layer calls `Repair`, which durably sets the torn bytes aside under a unique name before cutting back the log, then reloads. Repair does not delete any set-aside bytes.
- The first backend is `src/store/dir`: one directory per session, metadata plus an append-only JSON-lines log and `blobs/`. `store.compression` defaults to `zstd`, one frame per append, and `none` is available. The plain log closes each append with a commit line. Both are one write and one fsync per append. Files and their directories are synced when created. Existing sessions keep their format when the setting changes.
- Large outputs are stored by SHA-256 beside the log and referenced by `Part.Stored`. `PutBlob` is durable, deduplicates within a session, and `OpenBlob` checks the digest as the content is read. The model gets the preview part, not the stored reference.
- Everything a session needs is under its directory. Copy that directory under a new root, and a new process loads it, opens its blobs and continues. The backend test does exactly that. `Fork` copies the entire source history (ACP 1.31.0 has no point-in-history parameter), preserves entry ids, and uses the caller's new `Meta`. Blobs are hard linked because they are immutable, or copied across filesystems. Deleting either session leaves the other complete.
- `List` is ordered by id, oldest first. `Filter` combines Cwd, ParentID and archived state before applying its exclusive `After` cursor and caller-supplied `Limit` (zero is unlimited). ACP's omitted archive filter maps to `ArchivedExclude`; the store's zero filter matches all sessions. Parent lookup uses a rebuildable root-level children index, invalidated by root directory changes, not a scan of every session on every descendant lookup. The index is not session state and is rebuilt after directory copies.
- `dir.Store.Sweep` is called on demand at startup, not from `New`. It removes abandoned `.creating` and `.deleting` directories. Namespace changes take the root directory lock, and per-session operations take the log lock. No caller acquires the root lock while holding a log lock.
- `session/load` replays full history as `session/update` notifications. SQLite, Postgres, blob garbage collection and a plain-log offset index are not implemented.

Measured on Linux/btrfs/NVMe, 2000 turns and 36000 entries (58.9 MiB of JSON). One append per turn, reads on a fresh store value. The benchmark harness accepts any `store.Store` and measures appends, fsyncs, bytes written, disk size, load, history ranges and keyword scans. Full commands and limitations are in [the store note](notes/2026-11-11-store.md).

| measurement | plain | zstd |
|---|---|---|
| fsyncs per turn | 1 | 1 |
| disk size | 59.0 MiB | 20.6 MiB |
| p50 append per turn, compacted workload | 1.85 ms | 2.08 ms |
| load with no compaction | 444 ms | 360 ms |
| load after latest compaction (19 entries) | 371 µs | 409 µs |
| keyword scan, full history | 306 ms | 452 ms |

The final run had substantial fsync variance from concurrent system activity. Its first plain append pass averaged 11.8 ms, versus 2.78 ms for its compacted pass. The note reports both rather than treating that difference as a compression speedup.

At 10000 sessions, unfiltered `List` took 258 ms warm / 316 ms file-data-cold before paging. After the change its first 50 results take 4.71 / 5.46 ms. Full child lookup (100 matches) went from 243 / 314 ms to 2.52 / 3.89 ms. Initial children-index construction is separate: 126 ms with warm files. File-data-cold means `POSIX_FADV_DONTNEED` on the files, not evicted directory or inode caches.

## ACP mapping

| internal | ACP |
|---|---|
| text delta | `agent_message_chunk` |
| thinking delta | `agent_thought_chunk` |
| tool call | `tool_call` with kind, title, locations, raw input; `tool_call_update` with status, content, diffs |
| todo | `plan` |
| commands | `available_commands_update` |
| mode | `current_mode_update` |
| usage and cost | `usage_update` (context used, context size, cost) and `usage` on the prompt response |
| model, mode, thought level | `config_option_update` |
| active account, fallback, compaction, limit hit | `_meta.acpharness` on the relevant update, and the prompt response `_meta` |

Stop reasons map to ACP `end_turn`, `max_tokens`, `max_turn_requests`, `refusal`, `cancelled`.

ACP has no stop reason for a rate limit. A turn that stops for a reason the client can act on carries a typed `stop` under `acpharness` (`pkg/acp/harness`): `kind` is one of `rate_limit`, `auth`, `overload`, `context_full`, `limit`, `network`, `request`, `denied`, with `resetAt`, `retryAfterSeconds`, `limit`, `account` and `model` where they apply. It is in the prompt response's `_meta` when the turn has a stop reason (a limit answers `max_turn_requests`) and in the error's `data` when the turn failed. A client never has to parse error text. protocol7 treats `rate_limit` as a deferral. `denied` means a configured prompt hook refused the prompt: the turn did not start, the request answers `-32600`, and `message` is the hook's reason.

A chain with one entry does not wait out a rate limit: it reports it. In a pod, the workflow that owns account choice decides whether to wait or move.

## ACP layer

acpharness implements ACP itself and does not import `coder/acp-go-sdk`.

- Why: SDK v0.13.5 (2026-06-02) is generated from schema 0.13.5 and its master still pins that. The spec is at v1.17.0, schema 1.31.0 (2026-11-10). The SDK has no types for stable `compaction_update`, `compaction_summary_chunk`, `notice` and `systemPrompt`, and none for unstable `subagent_update`, `session/status` and `session/options`.
- Types: generated from the pinned schema files (`schema.json`, `schema.unstable.json`, `meta.json`) by a vendored copy of the SDK's generator (`cmd/generate`, Apache-2.0, about 3,700 lines, one dependency: `dave/jennifer`). We fix the generator where the new schema breaks it.
- Generator check, 2026-11-11: run unchanged against schema 1.31.0, it emitted every new type and update variant. Three generator bugs are fixed in our copy: `CreateElicitationRequest.Validate` read a `Message` field that the new union type does not have, every empty required string was rejected (so an empty file could not be written), and union marshalling carried unreachable code that `go vet` reports. `tools/acpgen/README.md` lists every change from upstream.
- Connection: our own JSON-RPC 2.0 code for newline-delimited messages (`pkg/acp/connection.go`). Each request runs on its own goroutine. Notifications and responses are handled in the order read. `$/cancel_request` cancels the named request's context. `Arrival(ctx)` gives a message's position in the read order, which is how the session owner orders a `session/cancel` or a new prompt against the running one. Details and reasons: `docs/notes/2026-11-11-acp-layer.md`.
- Capability gating: an update type is sent only when the client advertises it. Measured against SDK v0.13.5 on 2026-11-11: it decodes `compaction_summary_chunk` as a user message chunk, and decodes `compaction_update`, `notice` and `subagent_update` as a session info update. All of them pass its validation, so an old client misreads them without an error.
- The schema version is pinned in the repo. A bump is a reviewed regeneration.
- ACP v2 is in alpha (schema 2.0.0-alpha.15) and drops `session/load`, `session/set_mode` and `fs/*`. Owning the layer lets one binary serve both versions later.

## Ideas from Pi

From `earendil-works/pi` (MIT) at commit 4839e41, read 2026-11-11. Decided with the user on 2026-11-11.

Adopted:

- System prompt as named sections (see System prompt). Patching a changed section into the conversation is deferred.
- Tool input repair (see Extras), widened from Pi's edit tool to every tool.
- Entries with `id` and `parent_id` (see Sessions and storage). The parent field is written and unused.
- Compaction summary prompt (see Compaction): Pi's structured checkpoint (Goal, Constraints, Progress, Key Decisions, Next Steps, Critical Context) and its merge prompt are the default, with jcode's short prompt selectable. No measurement compares them yet.

Dropped:

- Steering and follow-up queues. jobshop already holds a message for a busy session until its turn ends, and its interrupt option cancels the turn first. The ACP prompt request has no steering field.

Later, undecided: MCP exposure levels per tool, virtual models, cache warming, prompt templates.

## Telemetry

- OpenTelemetry traces: span per prompt turn, provider request (model, account, tokens, retries, fallback), tool call, MCP call, compaction, subagent.
- Metrics: active sessions, turns, tokens by provider, account and model, cost, request latency, retries, fallbacks, tool latency and errors, compaction counts, RSS and goroutines.
- slog for logs. Stderr handler and OTel log handler are configured separately (level, format, enabled). Trace and span IDs are attached to records.
- OTLP endpoint and headers through the standard `OTEL_*` environment variables plus config.

## Code conventions

- Go 1.27.3 with `CGO_ENABLED=0`. Packages start in `src/`, with deliberate promotion to `pkg/` for external callers.
- JSON is `encoding/json/v2` with `jsontext`, enforced by lint. Per-message buffers come from `valyala/bytebufferpool` where a benchmark shows it saves memory. The measured behaviour is in `docs/notes/2026-11-11-contracts-and-conventions.md`.
- urfave/cli v3 plus koanf `cliflagv3` is the selected CLI and configuration integration.
- golangci-lint v2.15 with acphost's configuration and gofumpt.
- Plain Go concurrency: contexts for cancellation, one owner for session state, channels for coordination, and small scoped mutexes for shared state where needed. No blanket lock-free rule.
- Keep concrete types until a consumer needs an interface. Prefer small consumer-owned interfaces, explicit errors, and valid zero values where meaningful. Model lifecycle transitions explicitly without adding a sum-type framework.
- Limits come from caller configuration, protocol requirements or measurements. Configured provider fallback is intentional policy, not silent error recovery.
- Short dated notes in `docs/notes/` record decisions and dependency rationale. Comments explain non-obvious constraints.

## Workflow

- Commits go straight to master until the first milestone. After that: PRs with conventional titles, and release-please.
- CI runs on the `talos-metal-gfx` runner with acphost's steps: build, a `CGO_ENABLED=0` build, vet, race tests, golangci-lint v2.15.0, and a check that generated code is current.
- Parallel packages may be built by swarm workers once the core loop works: `claude-fable-5-2` at low effort on the `claude-panda` account.

## Release

- Private repo `gfx-labs/acpharness`, Go module `github.com/gfx-labs/acpharness`.
- release-please, static binaries (`CGO_ENABLED=0`, linux amd64 and arm64), Nix flake with the package and a NixOS or home-manager module later.
- Dependencies: koanf with `cliflagv3`, `urfave/cli/v3`, OTel Go SDK aligned with jobshop and `otelslog`, `modelcontextprotocol/go-sdk`, `gfx-labs/sse`, `valyala/bytebufferpool`, `coder/websocket`, `klauspost/compress`, `golang.org/x/sys`, `JohannesKaufmann/html-to-markdown/v2` for webfetch. Provider wire clients use `net/http`, `gfx-labs/sse` for SSE decoding, and `coder/websocket` for the OpenAI WebSocket transport. New dependencies need a recorded reason.

## Validation

- ACP compliance: a conformance suite driving the agent over each transport, with our own client and with `acp-go-sdk` v0.13.5 as the client jobshop uses today (initialize, new, prompt, cancel mid-tool, load and replay, set_config_option for model and mode, set_mode, permission in ask mode).
- Providers: recorded real responses per provider for the stream parser. Live checks against real subscriptions run by hand behind an env flag, not in CI. No tests against mocked providers.
- Tools: each dialect adapter is checked against outputs recorded from the pinned upstream CLI.
- Tests use testify, as acphost does.
- Task quality: about 10 real closed issues from acphost and jobshop, run on acpharness and jcode with the same model, comparing completion, turns, tokens and wall time.
- Footprint: RSS and CPU for 1, 10 and 200 idle and active sessions, compared with jcode, measured by a reproducible script.

## Implementation order

1. Repo scaffold, config, slog and OTel, ACP server on stdio with `initialize`, `session/new`, `session/prompt`, `session/cancel`, and a stub loop. Done 2026-11-11: `docs/notes/2026-11-11-acp-layer.md` and `docs/notes/2026-11-11-config-telemetry-server.md`.
2. Chat Completions provider (OpenRouter), generic dialect, agent loop with parallel tools, limits, cancellation. First real tasks run here.
3. Anthropic provider with OAuth login, refresh and locking, Claude dialect with pinned prompts.
4. Responses provider with Codex OAuth, Codex dialect, native compaction.
5. Model chains, fallback, retries, model switching through `session/set_config_option`, usage and cost reporting.
6. Session store interface with a first backend, `session/load`, store benchmark.
7. Extras: batch, output gating, search, todo and plan, web, subagents.
8. MCP meta-tool with all three transports and OAuth.
9. Compaction modes, embeddings client, Jev decisions, hooks, modes, slash commands, context loading options.
10. Unix and TCP listeners with mTLS, multi-session process mode.
11. Conformance suite, task quality comparison, footprint benchmark, then jobcity trial.

## Clients today

jobshop (origin/master 2b42e5fd, 2026-11-11) uses `acp-go-sdk` v0.13.5 and calls `initialize`, `session/new` with an empty MCP server list, `session/load`, `session/prompt`, `session/cancel` and `session/set_config_option`. acphost and jobshop now live under `gfx-labs`.

## Open questions

- Store default, decided by the benchmark in step 6.
- Claude Code's interruption message text, to be read from the pinned upstream version.
- Web search for chat providers without a built-in tool: none by default, user-supplied tool or MCP.
