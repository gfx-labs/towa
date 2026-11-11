# /// script
# requires-python = ">=3.12"
# dependencies = []
# ///
"""Capture model instructions, tool definitions and the interruption message
from openai/codex at one commit.

    uv run tools/capture/codex.py                       # the pinned commit
    uv run tools/capture/codex.py --ref <sha> --out <dir>

Text files are copied byte for byte. Tool definitions are not text upstream:
Rust code builds them per model and per feature set. They are produced by
compiling upstream's own planner with `codex_probe.rs` added as a test module
and serialising what it returns, so nothing here re-implements upstream logic.
That step needs `git`, `cargo` and the toolchain named in upstream's
`rust-toolchain.toml`, and builds codex-core (several minutes on first run).
"""

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent
sys.path.insert(0, str(HERE))

import outdir  # noqa: E402
UPSTREAM = "https://github.com/openai/codex"
PINNED = "ce0323f2f2d3b25e82806cd555237da3db4e1897"

# Copied unchanged into source/<upstream path>. A pattern that matches nothing
# is an error: upstream moved the file and the list needs a decision.
VERBATIM = [
    "LICENSE",
    "NOTICE",
    "codex-rs/models-manager/models.json",
    "codex-rs/models-manager/prompt.md",
    "codex-rs/protocol/src/prompts/base_instructions/default.md",
    "codex-rs/core/*_prompt.md",
    "codex-rs/prompts/templates/**/*",
    "codex-rs/collaboration-mode-templates/templates/*",
    "codex-rs/core/assets/tools/apply_patch.lark",
    "codex-rs/core/src/tools/handlers/*_spec.rs",
    "codex-rs/core/src/context/turn_aborted.rs",
    "codex-rs/core/src/tasks/mod.rs",
    # Which multi-agent version applies when the model catalog gives none;
    # that choice selects the interruption marker's role.
    "codex-rs/core/src/config/mod.rs",
    "codex-rs/ext/web-search/web_run_description.md",
    "codex-rs/ext/image-generation/imagegen_description.md",
]

MODELS_JSON = "codex-rs/models-manager/models.json"
GRAMMAR = "codex-rs/core/assets/tools/apply_patch.lark"
TURN_ABORTED = "codex-rs/core/src/context/turn_aborted.rs"
PROBE_PARENT = "codex-rs/core/src/tools/spec_plan.rs"
PROBE_FILE = "codex-rs/core/src/tools/acpharness_capture.rs"
PROBE_TEST = "tools::spec_plan::acpharness_capture::acpharness_capture"
PROFILES = ["default", "direct", "all_features_multi_agent_v1", "all_features_multi_agent_v2"]


class CaptureError(Exception):
    pass


def run(cmd, cwd, env=None):
    result = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True)
    if result.returncode != 0:
        raise CaptureError(f"{' '.join(map(str, cmd))} failed ({result.returncode}):\n{result.stdout[-4000:]}\n{result.stderr[-4000:]}")
    return result.stdout


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def dump(value) -> bytes:
    # Key order is the order of the serde_json::Value the probe serialised.
    return (json.dumps(value, indent=2, ensure_ascii=False) + "\n").encode("utf-8")


def checkout(work: Path, ref: str) -> tuple[Path, str]:
    clone = work / "codex"
    if not (clone / ".git").exists():
        work.mkdir(parents=True, exist_ok=True)
        run(["git", "clone", "--quiet", UPSTREAM, str(clone)], cwd=work)
    if subprocess.run(["git", "cat-file", "-e", f"{ref}^{{commit}}"], cwd=clone, capture_output=True).returncode != 0:
        run(["git", "fetch", "--quiet", "origin"], cwd=clone)
    commit = run(["git", "rev-parse", f"{ref}^{{commit}}"], cwd=clone).strip()
    # Discards an earlier interrupted run's probe.
    run(["git", "checkout", "--quiet", "--force", "--detach", commit], cwd=clone)
    (clone / PROBE_FILE).unlink(missing_ok=True)
    return clone, commit


def run_probe(clone: Path, work: Path) -> dict:
    parent = clone / PROBE_PARENT
    original = parent.read_bytes()
    out = work / "codex-probe.json"
    out.unlink(missing_ok=True)
    try:
        shutil.copyfile(HERE / "codex_probe.rs", clone / PROBE_FILE)
        parent.write_bytes(original + b'\n#[cfg(test)]\n#[path = "acpharness_capture.rs"]\nmod acpharness_capture;\n')
        env = {**os.environ, "ACPHARNESS_CAPTURE_OUT": str(out), "CARGO_TARGET_DIR": str(work / "codex-target")}
        run(
            ["cargo", "test", "--locked", "-p", "codex-core", "--lib", "--", PROBE_TEST, "--exact"],
            cwd=clone / "codex-rs",
            env=env,
        )
    finally:
        parent.write_bytes(original)
        (clone / PROBE_FILE).unlink(missing_ok=True)
    if not out.exists():
        raise CaptureError("the probe test did not run: upstream renamed the module it is attached to")
    return json.loads(out.read_text())


def rust_sources(clone: Path):
    for path in sorted((clone / "codex-rs").rglob("*.rs")):
        rel = path.relative_to(clone).as_posix()
        if "/tests/" in rel or rel.endswith("tests.rs") or "/target/" in rel:
            continue
        yield rel, path.read_text(encoding="utf-8", errors="replace").splitlines()


def declaration_sites(sources, name: str) -> list[str]:
    """file:line of each place a tool name is declared as a string literal."""
    literal = re.compile(r'(name: |_NAME: &str = |TOOL_NAME: &str = |ToolName::plain\(|ToolName::namespaced\([^,]+, )"' + re.escape(name) + '"')
    return [f"{rel}:{i}" for rel, lines in sources for i, line in enumerate(lines, 1) if literal.search(line)]


def spec_tool_names(spec: dict) -> list[str]:
    if spec.get("type") == "namespace":
        return [t["name"] for t in spec.get("tools", [])]
    return [spec["name"]] if "name" in spec else []


def capture(clone: Path, commit: str, probe: dict, out: Path):
    """Writes the capture into out, an empty scratch directory."""
    files = []

    def write(rel: str, data: bytes, source: dict):
        path = out / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        files.append({"path": rel, "sha256": sha256(data), "bytes": len(data), "source": source})

    # 1. Verbatim files.
    for pattern in VERBATIM:
        matched = sorted(p for p in clone.glob(pattern) if p.is_file())
        if not matched:
            raise CaptureError(f"no upstream file matches {pattern}")
        for path in matched:
            rel = path.relative_to(clone).as_posix()
            write(f"source/{rel}", path.read_bytes(), {"kind": "verbatim", "path": rel})

    # 2. Per-model instructions, the string values of models.json.
    models = json.loads((clone / MODELS_JSON).read_bytes())["models"]
    for index, model in enumerate(models):
        slug = model["slug"]
        if not re.fullmatch(r"[A-Za-z0-9._-]+", slug):
            raise CaptureError(f"model slug {slug!r} is not usable as a file name")
        messages = model.get("model_messages") or {}
        for field in ("instructions_template", "persistent_instructions"):
            value = messages.get(field)
            if value is None:
                continue
            write(
                f"instructions/{slug}.{field}.md",
                value.encode("utf-8"),
                {"kind": "json_string", "path": MODELS_JSON, "json_pointer": f"/models/{index}/model_messages/{field}"},
            )
        legacy = model.get("base_instructions")
        if legacy:
            write(
                f"instructions/{slug}.base_instructions.md",
                legacy.encode("utf-8"),
                {"kind": "json_string", "path": MODELS_JSON, "json_pointer": f"/models/{index}/base_instructions"},
            )

    # 3. Tool definitions from the probe.
    sources = list(rust_sources(clone))
    probe_source = {"kind": "probe", "path": "codex-rs/core/src/tools/spec_plan.rs", "method": "build_core_tool_registry, append_source_tools, ToolRouter::from_registry"}
    variants = {}
    for plan in probe["plans"]:
        for tool in plan["registered"]:
            key = ".".join(x for x in (tool["namespace"], tool["name"]) if x)
            digest = sha256(dump(tool["spec"]))
            group = variants.setdefault(key, {}).setdefault(digest, {"tool": tool, "seen": []})
            group["seen"].append({"model": plan["model"], "profile": plan["profile"], "exposure": tool["exposure"]})
    tool_index = {}
    for key in sorted(variants):
        groups = list(variants[key].values())
        with_default = [g for g in groups if any(s["profile"] == "default" for s in g["seen"])]
        used = set()
        for group in groups:
            first = min(group["seen"], key=lambda s: (PROFILES.index(s["profile"]), s["model"]))
            candidates = ([""] if with_default == [group] else []) + [f".{first['profile']}", f".{first['profile']}.{first['model']}"]
            suffix = next((c for c in candidates if c not in used), None)
            if suffix is None:
                raise CaptureError(f"cannot name the variants of tool {key}")
            used.add(suffix)
            rel = f"tools/specs/{key}{suffix}.json"
            tool = group["tool"]
            write(
                rel,
                dump(tool["spec"]),
                {
                    **probe_source,
                    "declared_at": sorted({site for name in spec_tool_names(tool["spec"]) for site in declaration_sites(sources, name)}),
                },
            )
            tool_index.setdefault(key, []).append(
                {
                    "file": rel,
                    "namespace": tool["namespace"],
                    "name": tool["name"],
                    "type": tool["spec"].get("type"),
                    "exposures": sorted({s["exposure"] for s in group["seen"]}),
                    "seen_in": sorted({f"{s['model']}/{s['profile']}" for s in group["seen"]}),
                }
            )

    request_index = []
    written = set()
    for plan in probe["plans"]:
        entry = {
            "model": plan["model"],
            "profile": plan["profile"],
            "tool_mode": plan["tool_mode"],
            "multi_agent_version": plan["multi_agent_version"],
            "use_responses_lite": plan["use_responses_lite"],
            "enabled_features": plan["enabled_features"],
        }
        for field, label in (("request_tools", "responses"), ("request_tools_responses_lite", "responses_lite")):
            data = dump(plan[field])
            rel = f"tools/requests/{label}.{sha256(data)[:12]}.json"
            if rel not in written:
                written.add(rel)
                write(rel, data, {**probe_source, "serializer": f"codex_tools::create_tools_json_for_{label}" if label == "responses_lite" else "codex_tools::create_tools_json_for_responses_api"})
            entry[label] = rel
        request_index.append(entry)

    # 4. The interruption message, rendered by upstream's own code.
    aborted_lines = (clone / TURN_ABORTED).read_text().splitlines()

    def line_of(needle):
        hits = [i for i, line in enumerate(aborted_lines, 1) if needle in line]
        if len(hits) != 1:
            raise CaptureError(f"{needle} found {len(hits)} times in {TURN_ABORTED}")
        return hits[0]

    interruption = {}
    for variant, constant in (("contextual_user", "INTERRUPTED_GUIDANCE"), ("developer", "INTERRUPTED_DEVELOPER_GUIDANCE")):
        item = probe["turn_aborted"][variant]
        content = item["content"]
        if len(content) != 1 or content[0]["type"] != "input_text":
            raise CaptureError(f"unexpected shape for the {variant} turn_aborted item")
        rel = f"interruption/turn_aborted.{variant}.txt"
        write(
            rel,
            content[0]["text"].encode("utf-8"),
            {
                "kind": "probe",
                "path": "codex-rs/core/src/tasks/mod.rs",
                "method": "interrupted_turn_history_marker",
                "text_declared_at": f"{TURN_ABORTED}:{line_of('const ' + constant + ':')}",
            },
        )
        interruption[variant] = {"file": rel, "role": item["role"], "item_type": item["type"]}

    commit_date = run(["git", "show", "-s", "--format=%cI", commit], cwd=clone).strip()
    toolchain = (clone / "codex-rs/rust-toolchain.toml").read_text()
    channel = re.search(r'channel\s*=\s*"([^"]+)"', toolchain)
    manifest = {
        "upstream": UPSTREAM,
        "commit": commit,
        "commit_date": commit_date,
        "captured_by": "tools/capture/codex.py",
        "rust_toolchain": channel.group(1) if channel else None,
        "profiles": {
            "default": "upstream's unit-test session (core/src/session/tests.rs make_session_and_context): API-key auth, default features, no MCP servers, no extensions, one local environment; the model's catalog entry decides the tool mode",
            "direct": "as default, with the model's tool_mode forced to direct, so every tool that code mode would nest is listed on its own",
            "all_features_multi_agent_v1": "direct, with every feature that core/src/tools/spec_plan.rs reads to register a core tool enabled, update_plan and request_user_input on, sleep always on, multi-agent v1",
            "all_features_multi_agent_v2": "as the previous profile with multi-agent v2",
        },
        "key_order": "JSON member order is what serde_json produced in codex-core's test build, where objects come out sorted by key. A release binary built with serde_json's preserve_order feature would send the same members in struct field order. Compare these files as JSON values, not as bytes of a request.",
        "interruption": interruption,
        "tools": tool_index,
        "requests": request_index,
        "not_captured": [
            "Tools contributed by extensions (web search `web.run`, image generation, goals, memories, skills, history notes, agent message board, multi-env file copy): the unit-test session installs no extensions. web_run_description.md and imagegen_description.md are copied as text; their schemas are not captured.",
            "MCP tools, dynamic tools, plugin install tools and `tool_search` contents: they depend on servers and plugins a session has.",
            "Windows variants: the probe ran on Linux, and exec_command adds shell guidance on Windows.",
            "Text assembled at run time around the instructions (environment context, AGENTS.md, permissions): only the template files under source/ are captured.",
        ],
        "files": sorted(files, key=lambda f: f["path"]),
    }
    (out / "manifest.json").write_bytes(dump(manifest))


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--ref", default=PINNED, help="upstream commit (default: the pinned one)")
    parser.add_argument("--work", type=Path, help="scratch directory for the clone and build (default: $JCODE_SCRATCH_DIR/capture or $TMPDIR)")
    parser.add_argument("--out", type=Path, help="output directory (default: prompts/codex/<first 8 of the commit>)")
    args = parser.parse_args()
    work = args.work or Path(os.environ.get("JCODE_SCRATCH_DIR") or os.environ.get("TMPDIR") or "/tmp") / "capture"
    try:
        clone, commit = checkout(work.resolve(), args.ref)
        out = (args.out or REPO / "prompts" / "codex" / commit[:8]).resolve()
        probe = run_probe(clone, work.resolve())
        # Built in scratch, then installed: the destination is never emptied.
        stage = Path(tempfile.mkdtemp(prefix=f"codex-{commit[:8]}-", dir=work.resolve()))
        try:
            capture(clone, commit, probe, stage)
            outdir.install(stage, out)
        finally:
            shutil.rmtree(stage)
    except (CaptureError, outdir.InstallError) as e:
        print(f"capture failed: {e}", file=sys.stderr)
        return 1
    print(f"wrote {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
