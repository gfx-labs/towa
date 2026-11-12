# Idle runtime footprint

2026-11-11, measured at 15:48–15:49 UTC.

## Scope and fixed build

Actual `cmd/towa`, built from `git archive` of **45ace318ef1a5606f2657a8e2ad5ac650ec4509f**, not the concurrently changing checkout. No application edits, injected runtime, Stub, prompt turns, provider requests, MCP server, new dependency or persistent benchmark seam.

- Host: Linux `6.12.115+deb13.1-amd64`, amd64, Intel Core i9-14900, 32 online logical CPUs, `/proc/meminfo` MemTotal 131616692 KiB. Scratch filesystem: btrfs. Other work was running on the host. No CPU pinning, cache eviction or frequency control.
- Toolchain: Go 1.27.3 linux/amd64, `CGO_ENABLED=0`, `GOAMD64=v1`, empty `GOEXPERIMENT`. Build: `go build -trimpath`, no race detector, no stripped symbols or other linker flags.
- Executable: 35735228 bytes, SHA-256 `9df484e84936b49f6917d187803b1f8156fb848fddfa4385b4a47b9224c7437e`. The archive build does not embed Git VCS metadata. Its source identity is the explicit archive hash above.
- Driver: Python 3.13.5 through uv 0.13.5, standard library only. Scratch `runtime-footprint/measure.py`, SHA-256 `a01744cda15ee7c5689054c200ffc6c61ecb4de48b72478a3045875592922f6a`.

Exact build and driver commands, from this repository, with a fresh scratch directory containing that driver:

```sh
scratch="$JCODE_SCRATCH_DIR/runtime-footprint"
mkdir -p "$scratch/source"
git archive 45ace318ef1a5606f2657a8e2ad5ac650ec4509f | tar -x -C "$scratch/source"
(cd "$scratch/source" && nice -n 10 env CGO_ENABLED=0 go build -trimpath -o ../towa ./cmd/towa)
cd "$scratch"
uv run --offline python measure.py --audit > audit-output.jsonl
uv run --offline python measure.py > output.jsonl
```

The driver refuses an existing per-run directory. It does not overwrite earlier measurements. Source, binary, driver, ACP exchanges, environment/config snapshots, session stores, network traces and result JSON all remain in `/home/a/.jcode/scratch/runtime-footprint/`, not in the repository. `results.json` contains the untraced runs and `audit-results.json` the two separate traced runs.

## Workload and isolation

Five interleaved fresh-process pairs: **1, 50, 1, 50, 1, 50, 1, 50, 1, 50** idle ACP sessions. Fifty matches the session count in `2026-11-11-mcp.md`, not its workload: that earlier measurement constructed managers and called an MCP tool, while this one drives the actual binary over ACP and leaves MCP off. These are not comparable performance baselines.

Each process receives one JSON-RPC `initialize`, waits for its acknowledgement, then creates sessions sequentially with `session/new`, waiting for each acknowledgement. Exact request shapes, where `cwd` is the fresh absolute per-run work directory:

```json
{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{},"clientInfo":{"name":"idle-footprint","version":"1"}}}
{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"<run>/work","mcpServers":[]}}
```

Session request IDs run from 1 through the requested session count. No `session/prompt` or tool call is sent. All returned session IDs must be unique. Before sampling and again after clean shutdown, the driver verifies an exact directory-ID match, every `meta.json` ID/cwd/model (`idle`), and every `log.jsonl.zst` file. There is no replacement response or fallback if setup fails.

The child environment is replaced, not extended: `PATH=/usr/bin:/bin`, `LANG=C.UTF-8`, a dummy `IDLE_DUMMY_KEY=not-a-real-key`, and fresh absolute HOME, TMPDIR and all of XDG_CONFIG_HOME, XDG_STATE_HOME, XDG_CACHE_HOME, XDG_DATA_HOME and XDG_RUNTIME_DIR. Process cwd and session cwd are the same empty `<run>/work`. No proxy, credential, TOWA or OTel environment is inherited. `/etc/towa` was absent. The explicit config is `<run>/config.json`, with this content apart from the absolute store path and dynamically allocated loopback trap port:

```json
{
  "default_model": "idle",
  "models": {"idle": {
    "chain": ["local:dummy/idle-model"],
    "context_window": 131072, "max_output_tokens": 1024,
    "dialect": "generic", "family": "generic"
  }},
  "providers": [{"id": "local", "kind": "chat", "base_url": "http://127.0.0.1:<trap-port>/v1"}],
  "accounts": [{"id": "dummy", "provider": "local", "kind": "api_key", "api_key_env": "IDLE_DUMMY_KEY"}],
  "store": {"root": "<run>/store", "compression": "zstd"},
  "compaction": {"mode": "off", "openai_native": "off"},
  "context": {"default_agents": false, "skills_enabled": false, "default_skills": false},
  "webfetch": {"enabled": false},
  "conversation_search": {"enabled": false},
  "subagents": {"max_depth": 0},
  "jev": {"route": ""},
  "embeddings": {"base_url": ""},
  "tools": {"repair": {"enabled": false}, "gate": {"enabled": false}, "batch": {"enabled": false}},
  "mcp": {"servers": []}, "hooks": [],
  "telemetry": {"otel": false, "logs": {"stderr": {"enabled": false}}},
  "listen": {"stdio": true, "unix": {"path": ""}, "tcp": {"address": ""}}
}
```

The driver launches `[<scratch>/towa, --config, <run>/config.json, serve]` directly with `subprocess.Popen`, that environment, and cwd `<run>/work`. Core generic tools and the normal model/router/runtime assembly are real. The configured context/output sizes are explicit idle-workload inputs, not measured metadata for an actual model. Extras, compaction, embeddings, exports, hooks and MCP servers are disabled, not removed from the binary.

## Sampling and checks

- Startup elapsed: immediately before opening stderr/spawning the process through receipt of the successful `initialize` response.
- Creation elapsed: immediately before the first `session/new` through the final successful response. Includes ACP handling, real session/runtime/tool setup and durable store work. It excludes subsequent driver-side store verification.
- After creation and store verification, wait one second without traffic, sample `/proc/<pid>`, wait three seconds, sample again. No forced GC. The table reports the second memory sample.
- RSS and PSS: `Rss` and `Pss` from `smaps_rollup`, in KiB. `status` VmRSS and thread count are also retained. The driver reads `stat` fields 14/15 for process user/system ticks at both ends. `SC_CLK_TCK=100` on this host. Idle windows were 3.0021–3.0025 seconds including sampling overhead.
- Every sample checks all live thread `children` files: no subprocess was present. Driver, loopback trap and any tracer are outside the target process's memory/CPU figures.
- Shutdown: close stdin, wait for normal process exit, require exit code zero, verify stored sessions again. No kill was needed. Reported shutdown elapsed includes the Python wait/poll overhead, not just application teardown.

Every untraced process acknowledged/stored all requested sessions: 255 total. Two additional fresh-process network audits acknowledged/stored another 51, giving **306 verified sessions**, all clean EOF exits. No ACP error occurred. The dummy provider's local trap counted **zero TCP connections and zero HTTP requests in every run**, including audits. No external endpoint was configured or contacted.

The two audits prepend `/usr/bin/strace -f -e trace=network -o <run>/network.log` to the exact child invocation, with workloads 1 and 50. The saved traces have no named socket/connect/send/bind/listen/accept operations. They contain Go runtime signals and process exits. One trace ends an unfinished undecoded syscall at shutdown. These audit timings are excluded from the measurement table, and no claim is made that tracing has no overhead.

## Results

Rows are in execution order. All rows have user/system idle tick deltas **0/0** and request count **0**.

| Run | Sessions | Startup ms | Creation ms | RSS KiB | PSS KiB | Threads | EOF to exit ms |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 0 | 1 | 7.135 | 18.971 | 23392 | 23384 | 13 | 3.206 |
| 1 | 50 | 7.048 | 351.536 | 25832 | 25824 | 15 | 3.185 |
| 2 | 1 | 6.590 | 9.731 | 23592 | 23584 | 14 | 3.200 |
| 3 | 50 | 6.715 | 346.982 | 28696 | 28688 | 15 | 3.180 |
| 4 | 1 | 6.797 | 9.558 | 23060 | 23052 | 14 | 3.228 |
| 5 | 50 | 6.356 | 319.991 | 25580 | 25572 | 15 | 1.117 |
| 6 | 1 | 6.819 | 17.768 | 25004 | 24996 | 13 | 1.117 |
| 7 | 50 | 6.702 | 352.635 | 25808 | 25800 | 14 | 3.176 |
| 8 | 1 | 7.324 | 9.801 | 25328 | 25320 | 13 | 3.174 |
| 9 | 50 | 6.526 | 328.368 | 26568 | 26560 | 15 | 3.209 |

Observed ranges:

| Sessions | Startup ms | Creation ms | RSS KiB | PSS KiB |
|---:|---:|---:|---:|---:|
| 1 | 6.590–7.324 | 9.558–18.971 | 23060–25328 | 23052–25320 |
| 50 | 6.356–7.048 | 319.991–352.635 | 25580–28696 | 25572–28688 |

Zero sampled CPU tick change is not proof of zero CPU use. Tick resolution, a short idle interval and sampling can hide small costs. No throughput, active-turn, tool execution, embeddings, compaction, MCP connection, transport pool or long-lived retention measurement was made.

These are absolute observations of one fixed build and configuration on a shared host, not an optimization result or comparison with another harness. File caches, GC/runtime scheduling, btrfs storage and concurrent work can affect both memory and elapsed time. In this first measurement no zero-session or earlier-build baseline was measured, so the difference between rows is not an isolated per-session cost. RSS includes shared/file-backed mappings. PSS apportions shared mappings but is not total system memory consumption. No sum of RSS is reported as physical memory.

## Validator-migration build comparison

Bounded follow-up at 16:05–16:06 UTC, same host and toolchain. The original measurements above remain unchanged. This comparison reuses their exact binary as `old`, and builds the committed validator migration **c9c09c4f29b6b63697dc260b535b493230578216** as `new`. The latter adds `github.com/go-playground/validator/v10 v10.30.6`. Its executable is 37504695 bytes, SHA-256 `e51e04eadb91a42658f4f4f006679ac6f33cb0cf4709e087eea2926c6c085742`. The whole-build file-size difference is +1769467 bytes.

This is **not an isolated measurement of validator's cost**. Between these two source hashes, captured Claude Read/Bash adapters and generic tool descriptions changed, and the loop gained terminal compaction consumption. Source inspection identified these confounds before interpreting the measurements. No control build reverting only validator was made, and no application or module files were edited for the experiment.

Scratch `validator-comparison/compare.py` (SHA-256 `d241a2c891dae9e8f440db94a6dcc6bc591e6a0c191c1255f38e238c1c732e96`) imports the unchanged `measure.py` above and calls its existing `run`. It changes only the artifact root and executable path. It runs five pairs for each session count and each build, twenty fresh processes total. Pair 0 runs old/new with 1 session, then old/new with 50. Pair 1 reverses the build order to new/old for both counts, alternating thereafter. The five-pair limit, sequential ACP calls, one-second settling, three-second idle sampling and EOF shutdown policy are unchanged. No extra traced workload was added.

Exact commands, with fresh comparison directories and that scratch caller present:

```sh
scratch="$JCODE_SCRATCH_DIR/runtime-footprint"
comparison="$scratch/validator-comparison"
mkdir -p "$comparison/old" "$comparison/new" "$comparison/source"
cp "$scratch/towa" "$comparison/old/towa"
git archive c9c09c4f29b6b63697dc260b535b493230578216 | tar -x -C "$comparison/source"
(cd "$comparison/source" && nice -n 10 env CGO_ENABLED=0 GOPROXY=off GOSUMDB=off go build -trimpath -o ../new/towa ./cmd/towa)
cd "$comparison"
uv run --offline python compare.py > output.jsonl
```

Both builds use the same CGO-disabled, unstripped `-trimpath` options. Offline module settings prevent dependency fetches during the candidate build. Both archived `go.mod`/`go.sum` files were verified unchanged after building. The copied baseline binary's SHA-256 still matches the original. Config content is identical after replacing the per-build store-directory component. Environment isolation, dummy credential, endpoint trap, generic model, extras/compaction/MCP-off settings and actual runtime/store checks are unchanged.

All **510 sessions** were acknowledged and verified in the store before and after normal exit. All twenty processes exited zero on EOF, with no child processes at either sample. Every run had **zero trap connections and zero HTTP requests**, and user/system idle tick changes **0/0** at 100 Hz. No prompt, inference, module download or external endpoint call was made. Detailed ACP/config/environment/store/result artifacts remain under scratch `validator-comparison/{old,new}` and the combined data is `validator-comparison/results.json`.

Raw paired observations, with startup in milliseconds and memory in KiB:

| Pair | Sessions | Startup old | Startup new | RSS old | RSS new | PSS old | PSS new |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 0 | 1 | 6.735 | 8.195 | 22884 | 26236 | 22876 | 26228 |
| 1 | 1 | 6.616 | 9.139 | 21580 | 24312 | 21572 | 24304 |
| 2 | 1 | 6.528 | 7.665 | 23976 | 24276 | 23968 | 24268 |
| 3 | 1 | 7.656 | 7.338 | 22792 | 24572 | 22784 | 24564 |
| 4 | 1 | 6.694 | 8.101 | 25528 | 25972 | 25520 | 25964 |
| 0 | 50 | 6.997 | 7.975 | 25788 | 27684 | 25780 | 27676 |
| 1 | 50 | 6.679 | 7.859 | 25748 | 27788 | 25740 | 27780 |
| 2 | 50 | 7.442 | 8.327 | 25996 | 28076 | 25988 | 28068 |
| 3 | 50 | 7.584 | 7.475 | 25784 | 27972 | 25776 | 27964 |
| 4 | 50 | 7.079 | 7.106 | 28256 | 28240 | 28248 | 28232 |

Ranges and median of the five paired differences (`new - old`, not difference of medians):

| Sessions | Build | Startup range ms | RSS range KiB | PSS range KiB |
|---:|---|---:|---:|---:|
| 1 | old | 6.528–7.656 | 21580–25528 | 21572–25520 |
| 1 | new | 7.338–9.139 | 24276–26236 | 24268–26228 |
| 50 | old | 6.679–7.584 | 25748–28256 | 25740–28248 |
| 50 | new | 7.106–8.327 | 27684–28240 | 27676–28232 |

| Sessions | Median paired startup delta ms | Median paired RSS delta KiB | Median paired PSS delta KiB |
|---:|---:|---:|---:|
| 1 | +1.407 | +1780 | +1780 |
| 50 | +0.884 | +2040 | +2040 |

The new whole build usually started later and retained more resident memory in these runs, but the observed ranges overlap. Each count has one negative startup difference. The final 50-session memory difference is -16 KiB, rather than an increase. Baseline one-session RSS itself varies by 3948 KiB here. The 50-session creation intervals also varied: old 328.941–755.990 ms, new 347.442–483.062 ms. These data do not establish a stable creation-time improvement or a deterministic memory increment.

The paired medians describe this small sample, not a confidence interval, isolated dependency overhead or long-running CPU result. They support reporting the migration build's observed footprint, not assigning all of the difference to validator. No further runs, tuning or production changes were made to reduce noise or improve the result.
