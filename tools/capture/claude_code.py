# /// script
# requires-python = ">=3.12"
# dependencies = ["tree-sitter==0.25.2", "tree-sitter-javascript==0.25.0", "regex==2026.11.9"]
# ///
"""Capture system prompt sections, tool definitions and the interruption
messages from a published Claude Code release, without installing or running it.

    uv run tools/capture/claude_code.py                 # the pinned version
    uv run tools/capture/claude_code.py --version 2.1.304 --out <dir>

`@anthropic-ai/claude-code` is a launcher. The program is a Bun single-file
executable in the platform package `@anthropic-ai/claude-code-linux-x64`. Both
tarballs are fetched with `npm pack`, the executable's embedded JavaScript is
read with bunfs.py, and the texts are computed by jseval.py, which evaluates
the bundle's own prompt builders and refuses anything that depends on a running
session. Where a text depends on a condition that only a session can decide,
every outcome is written as its own variant and the manifest records the
condition and the answer for each.

The same tarballs always give the same output.
"""

import argparse
import base64
import hashlib
import io
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import threading
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import bunfs  # noqa: E402
import jseval  # noqa: E402
import outdir  # noqa: E402
import zodschema  # noqa: E402
from jseval import UNDEF, Env, Function, JSObject, Unknown, Unresolved  # noqa: E402

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent
PINNED = "2.1.280"
LAUNCHER = "@anthropic-ai/claude-code"
PLATFORM = "@anthropic-ai/claude-code-linux-x64"
EXECUTABLE = "package/claude"
SDK_TYPES = "package/sdk-tools.d.ts"
# The entries of the output directory this script writes. profiles/ beside
# them holds recorded captures written by claude_code_request.py.
OWNED_OUTPUT = ("interruption", "system", "tools", "package", "manifest.json")

# Budget for one item: the combinations of undecided conditions that are
# evaluated, and the distinct texts written out. Over either, the item is
# listed as not captured. Neither shortens a result.
MAX_COMBINATIONS = 48
MAX_VARIANTS = 48

INTERRUPT_PREFIX = "[Request interrupted by user"
IDENTITY_PREFIXES = ("You are Claude Code, Anthropic's official CLI for Claude", "You are a Claude agent, built on Anthropic's Claude Agent SDK")
# The string that names one of the system prompt sections, used to find the
# function that assembles the prompt.
ASSEMBLER_MARKER = b"env_info_simple"


class CaptureError(Exception):
    pass


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def dump(value) -> bytes:
    return (json.dumps(value, indent=2, ensure_ascii=False) + "\n").encode("utf-8")


def fetch(package: str, version: str, work: Path) -> Path:
    name = package.lstrip("@").replace("/", "-") + f"-{version}.tgz"
    path = work / name
    if not path.exists():
        work.mkdir(parents=True, exist_ok=True)
        result = subprocess.run(
            ["npm", "pack", "--silent", "--ignore-scripts", f"{package}@{version}"], cwd=work, capture_output=True, text=True
        )
        if result.returncode != 0 or not path.exists():
            raise CaptureError(f"npm pack {package}@{version} failed: {result.stderr.strip()}")
    return path


def member(tarball: bytes, name: str) -> bytes:
    with tarfile.open(fileobj=io.BytesIO(tarball), mode="r:gz") as tar:
        f = tar.extractfile(name)
        if f is None:
            raise CaptureError(f"{name} is not a file in the tarball")
        return f.read()


def walk(node):
    stack = [node]
    while stack:
        n = stack.pop()
        yield n
        stack.extend(reversed(n.children))


def object_members(node):
    out = {}
    for c in node.named_children:
        key = c.child_by_field_name("key") if c.type == "pair" else c.child_by_field_name("name") if c.type == "method_definition" else None
        if key is not None:
            out[key.text.decode("utf-8")] = c
    return out


def short(name: str) -> str:
    return name.removeprefix(bunfs.ROOT)


def describe_trace(trace):
    return [
        {
            "condition": d["source"] if len(d["source"]) <= 240 else d["source"][:240] + "...",
            "question": d["question"],
            "depends_on": d["depends_on"],
            "answer": d["answer"],
            "module": short(d["module"]),
            "offset": d["offset"],
        }
        for d in trace
    ]


class Capture:
    def __init__(self, sources: dict, out: Path):
        self.sources = sources
        self.interp = jseval.Interp(sources, "linux")
        self.out = out
        self.files = []
        self.items = {}
        self.not_captured = []

    def write(self, rel: str, data: bytes, located: dict):
        path = self.out / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        self.files.append({"path": rel, "sha256": sha256(data), "bytes": len(data), "located": located})

    def miss(self, kind: str, name: str, reason: str, located: dict):
        self.not_captured.append({"kind": kind, "name": name, "reason": reason, "located": located})

    def variants(self, compute):
        """[(result, [trace, ...])] for each distinct result, in first-seen order.

        Raises Unresolved when any combination of conditions fails: a text that
        is only known for some outcomes is not written at all."""
        results = []
        for trace, result in self.interp.variants(compute, MAX_COMBINATIONS):
            if isinstance(result, Unresolved):
                path = ">".join(getattr(result, "path", [])[-5:])
                raise Unresolved(f"{result}" + (f" (in {path})" if path else ""))
            key = json.dumps(result, sort_keys=False, ensure_ascii=False)
            for existing in results:
                if existing[0] == key:
                    existing[2].append(trace)
                    break
            else:
                results.append([key, result, [trace]])
                if len(results) > MAX_VARIANTS:
                    raise Unresolved(f"more than {MAX_VARIANTS} distinct texts over the undecided conditions")
        return [(result, traces) for _, result, traces in results]

    def write_variants(self, base: str, ext: str, produced, encode, located: dict, kind: str, name: str):
        """produced: [(result, traces, labels)] with distinct results."""
        entry = []
        for index, (result, traces, labels) in enumerate(produced, 1):
            rel = f"{base}{ext}" if len(produced) == 1 else f"{base}.v{index}{ext}"
            self.write(rel, encode(result), located)
            entry.append({"file": rel, **labels, "when": [describe_trace(t) for t in traces]})
        self.items.setdefault(kind, {})[name] = entry

    # -- interruption ----------------------------------------------------

    def interruption(self):
        found = {}
        lists = []
        for name in sorted(self.sources):
            source = self.sources[name]
            if INTERRUPT_PREFIX.encode() not in source:
                continue
            module = self.interp.module(name)
            for node in walk(module.tree.root_node):
                if node.type == "variable_declarator":
                    value = node.child_by_field_name("value")
                    if value is None:
                        continue
                    if value.type == "string":
                        text = self.interp.eval(value, Env(), module)
                        if text.startswith(INTERRUPT_PREFIX):
                            found.setdefault(text, []).append((name, node))
                    elif value.type == "array" and value.named_children and all(c.type == "string" for c in value.named_children):
                        items = [self.interp.eval(c, Env(), module) for c in value.named_children]
                        if any(i.startswith(INTERRUPT_PREFIX) for i in items):
                            lists.append((name, node, items))
        if not found:
            raise CaptureError("no interruption message constant found")
        entries = []
        for text in sorted(found):
            sites = found[text]
            slug = re.sub(r"[^a-z0-9]+", "_", text.lower()).strip("_")
            rel = f"interruption/{slug}.txt"
            self.write(
                rel,
                text.encode("utf-8"),
                {
                    "how": f"string literal starting with {INTERRUPT_PREFIX!r} assigned to a module variable",
                    "sites": [{"module": short(m), "offset": n.start_byte, "identifier": n.child_by_field_name("name").text.decode()} for m, n in sites],
                },
            )
            entries.append({"file": rel, "text": text})
        # The array that upstream uses to recognise its own cancellation texts.
        seen = set()
        for index, (name, node, items) in enumerate(lists):
            key = json.dumps(items)
            if key in seen:
                continue
            seen.add(key)
            rel = "interruption/cancellation_texts.json" if not index else f"interruption/cancellation_texts.{index}.json"
            self.write(
                rel,
                dump(items),
                {
                    "how": "array of string literals that includes an interruption message; upstream tests a user message against it to tell a synthetic cancellation from typed text",
                    "sites": [{"module": short(name), "offset": node.start_byte, "identifier": node.child_by_field_name("name").text.decode()}],
                },
            )
        self.items["interruption"] = entries

    # -- identity --------------------------------------------------------

    def identity(self):
        found = {}
        for name in sorted(self.sources):
            source = self.sources[name]
            if not any(p.encode() in source for p in IDENTITY_PREFIXES):
                continue
            module = self.interp.module(name)
            for node in module.tree.root_node.children:
                if node.type not in ("variable_declaration", "lexical_declaration"):
                    continue
                for d in node.named_children:
                    value = d.child_by_field_name("value")
                    if value is not None and value.type == "string":
                        text = self.interp.eval(value, Env(), module)
                        if text.startswith(IDENTITY_PREFIXES):
                            found.setdefault(text, []).append((name, d))
        entries = []
        for index, text in enumerate(sorted(found), 1):
            rel = f"system/identity.{index}.txt"
            self.write(
                rel,
                text.encode("utf-8"),
                {
                    "how": "top-level string constant starting with the product identity sentence",
                    "sites": [{"module": short(m), "offset": n.start_byte, "identifier": n.child_by_field_name("name").text.decode()} for m, n in found[text]],
                },
            )
            entries.append({"file": rel, "text": text})
        if not entries:
            self.miss("system", "identity", "no identity constant found", {})
        self.items["identity"] = entries

    # -- tools -----------------------------------------------------------

    def tool_definitions(self):
        for name in sorted(self.sources):
            source = self.sources[name]
            if b"inputSchema" not in source and b"inputJSONSchema" not in source:
                continue
            module = self.interp.module(name)
            for node in walk(module.tree.root_node):
                if node.type != "object":
                    continue
                members = object_members(node)
                if "name" in members and "prompt" in members and ("inputSchema" in members or "inputJSONSchema" in members):
                    yield module, node

    def tools(self):
        seen = {}
        for module, node in self.tool_definitions():
            located = {"how": "object literal with `name`, `prompt` and `inputSchema` members", "module": short(module.name), "offset": node.start_byte}
            try:
                obj = self.interp.eval(node, Env(), module)
                name = jseval.to_string(obj.get("name"))
            except Unresolved as e:
                name_node = object_members(node)["name"].child_by_field_name("value")
                label = name_node.text.decode("utf-8") if name_node is not None else "?"
                self.miss("tool", f"<name expression {label}>", f"the tool name is computed at run time: {e}", located)
                continue
            if not re.fullmatch(r"[A-Za-z0-9_.-]+", name):
                self.miss("tool", name, "the tool name is not usable as a file name", located)
                continue
            if name in seen:
                self.miss("tool", name, f"a second definition with this name (the first is in {seen[name]['module']} at {seen[name]['offset']}); only the first is captured", located)
                continue
            seen[name] = located
            try:
                produced = self.tool(module, node, name)
            except Unresolved as e:
                self.miss("tool", name, str(e), located)
                continue
            self.write_variants(f"tools/{name}", ".json", produced, dump, located, "tools", name)

    def tool(self, module, node, name):
        results = []

        def compute(lean):
            # A fresh object per run: its members are evaluated lazily and
            # must not carry a value over from another combination.
            obj = self.interp.eval(node, Env(), module)
            context = JSObject()
            context.props["leanPrompt"] = lean
            for key in ("model", "tools", "agents", "preReadLineDropped", "bashFirstTrimmed", "delegationCostsFirst", "delegationCostsFirstText", "getToolPermissionContext"):
                context.props[key] = Unknown(f"the `{key}` given to the tool's prompt()", origin=("prompt context", key))
            prompt = obj.get("prompt")
            description = self.interp.call(prompt, obj, [context], "prompt")
            if not isinstance(description, str):
                raise Unresolved(f"prompt() returned {type(description).__name__}, not a constant string")
            ready = obj.get("inputJSONSchema")
            if ready is not UNDEF:
                schema = jseval.to_python(ready)
            else:
                declared = obj.get("inputSchema")
                if isinstance(declared, Function):
                    declared = declared.call(obj, [])
                if not isinstance(declared, zodschema.Schema):
                    raise Unresolved(f"inputSchema is {type(declared).__name__}, not a schema built from constants")
                schema = self.interp.zod.to_json_schema(declared)
            return {"name": name, "description": description, "input_schema": schema}

        for lean in (True, False):
            for result, traces in self.variants(lambda: compute(lean)):
                for existing in results:
                    if existing[0] == result:
                        existing[1].extend(traces)
                        existing[2]["lean_prompt"].append(lean)
                        break
                else:
                    results.append((result, list(traces), {"lean_prompt": [lean]}))
        return results

    # -- system prompt ---------------------------------------------------

    def system(self):
        candidates = []
        for name in sorted(self.sources):
            if ASSEMBLER_MARKER not in self.sources[name]:
                continue
            module = self.interp.module(name)
            for node in module.tree.root_node.children:
                if node.type == "function_declaration" and ASSEMBLER_MARKER in node.text:
                    candidates.append((module, node))
        if len(candidates) != 1:
            self.miss("system", "sections", f"expected one function containing {ASSEMBLER_MARKER.decode()!r}, found {len(candidates)}", {})
            return
        module, fn = candidates[0]
        fn_name = fn.child_by_field_name("name").text.decode()
        located = {"how": f"the top-level function that contains the section name {ASSEMBLER_MARKER.decode()!r}", "module": short(module.name), "offset": fn.start_byte, "identifier": fn_name}

        def scope():
            # The assembler's parameters and locals are session state.
            env = Env()
            for n in walk(fn.child_by_field_name("parameters")):
                if n.type == "identifier":
                    env.vars[n.text.decode()] = Unknown(f"parameter {n.text.decode()} of the prompt assembler", origin=("assembler", n.text.decode()))
            for n in walk(fn.child_by_field_name("body")):
                if n.type == "variable_declarator" and n.parent.parent is not None:
                    for bound in jseval.pattern_names(n.child_by_field_name("name")):
                        env.vars.setdefault(bound, Unknown(f"local {bound} of the prompt assembler", origin=("assembler", bound)))
            return env

        # Named sections: calls of the form f("<name>", () => text).
        named = []
        for n in walk(fn.child_by_field_name("body")):
            if n.type != "call_expression":
                continue
            args = [a for a in n.child_by_field_name("arguments").named_children if a.type != "comment"]
            if len(args) == 2 and args[0].type in ("string", "template_string") and args[1].type == "arrow_function":
                named.append((args[0], args[1]))
        # Leading sections: the call expressions in the array the function returns.
        leading = []
        for n in walk(fn.child_by_field_name("body")):
            if n.type == "return_statement" and n.named_children and n.named_children[0].type == "call_expression":
                target = n.named_children[0]
                arrays = [a for a in walk(target) if a.type == "array"]
                if arrays:
                    leading = [c for c in walk(arrays[0]) if c.type == "call_expression" and c.child_by_field_name("function").type == "identifier"]
        if not named:
            self.miss("system", "sections", "no named section calls found in the assembler", located)
            return

        order = []
        for index, call in enumerate(leading):
            callee = call.child_by_field_name("function").text.decode()
            label = f"leading.{index:02d}"
            self.section(label, lambda env, c=call: self.interp.eval(c, env, module), scope, {**located, "expression": call.text.decode("utf-8")[:120], "builder": callee}, order)
        for index, (name_node, arrow) in enumerate(named):
            raw = name_node.text.decode("utf-8")
            static = re.match(r"""^[`"']([A-Za-z0-9_-]+)""", raw)
            key = static.group(1) if static else f"section{index:02d}"
            label = f"section.{index:02d}.{key}"

            def compute(env, a=arrow):
                thunk = self.interp.eval(a, env, module)
                return thunk.call(UNDEF, [])

            self.section(label, compute, scope, {**located, "section_name_expression": raw[:160]}, order)
        self.items["system_order"] = order

    def section(self, label, compute, scope, located, order):
        def run():
            value = compute(scope())
            if value is None or value is UNDEF:
                return None
            if isinstance(value, list):
                value = [v for v in value if v is not None and v is not UNDEF]
                if not all(isinstance(v, str) for v in value):
                    raise Unresolved("a section returned a list with a value that is not a constant string")
                return value
            if not isinstance(value, str):
                raise Unresolved(f"the section is {type(value).__name__}, not a constant string")
            return value

        try:
            found = self.variants(run)
        except Unresolved as e:
            self.miss("system", label, str(e), located)
            order.append({"section": label, "captured": False})
            return
        texts = [(v, t) for v, t in found if v is not None]
        absent = [trace for v, traces in found if v is None for trace in traces]
        produced = [(v, t, {}) for v, t in texts]
        if produced:
            encode = lambda v: (v if isinstance(v, str) else "\n".join(v)).encode("utf-8")  # noqa: E731
            self.write_variants(f"system/{label}", ".txt", produced, encode, located, "system", label)
            if absent:
                self.items["system"][label].append({"file": None, "note": "the section is absent", "when": [describe_trace(t) for t in absent]})
        order.append({"section": label, "captured": bool(produced), "always_absent": not produced})


def run(version: str, work: Path, out: Path):
    launcher = fetch(LAUNCHER, version, work).read_bytes()
    platform = fetch(PLATFORM, version, work).read_bytes()
    executable = member(platform, EXECUTABLE)
    modules = bunfs.modules(executable)
    sources = {m.name: m.data for m in modules if m.name.endswith(".js")}
    if not sources:
        raise CaptureError("no JavaScript modules in the executable")
    stamp = f"// Version: {version}\n".encode()
    if not any(stamp in data for data in sources.values()):
        raise CaptureError(f"no module carries the banner for version {version}")

    # Built in the scratch directory, then installed. The destination is read
    # by other tools while this runs for a minute, so it is never emptied.
    work.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=f"static-{version}-", dir=work))
    try:
        manifest = build(version, launcher, platform, executable, modules, sources, stage)
        outdir.install(stage, out, OWNED_OUTPUT)
    finally:
        shutil.rmtree(stage)
    return manifest


def build(version, launcher, platform, executable, modules, sources, out: Path):
    capture = Capture(sources, out)
    capture.write(
        "package/sdk-tools.d.ts",
        member(launcher, SDK_TYPES),
        {"how": f"file {SDK_TYPES} of the {LAUNCHER} tarball, copied unchanged: upstream's generated TypeScript types for every tool input and output"},
    )
    capture.interruption()
    capture.identity()
    capture.tools()
    capture.system()

    def integrity(data):
        return "sha512-" + base64.b64encode(hashlib.sha512(data).digest()).decode()

    manifest = {
        "upstream": LAUNCHER,
        "version": version,
        "captured_by": "tools/capture/claude_code.py",
        "packages": [
            {"name": LAUNCHER, "version": version, "tarball_integrity": integrity(launcher)},
            {"name": PLATFORM, "version": version, "tarball_integrity": integrity(platform), "executable": EXECUTABLE, "executable_sha256": sha256(executable)},
        ],
        "bundle": {"embedded_files": len(modules), "javascript_modules": len(sources)},
        "method": {
            "extraction": "The executable is not run. Its ELF section .bun holds Bun's module graph; bunfs.py reads the embedded files from it.",
            "evaluation": "jseval.py parses the modules with tree-sitter and evaluates upstream's own builder functions over constants. A value that only a running session has is unknown. A condition on an unknown value is taken both ways, and each distinct text is written as a variant with the conditions and answers under `when`. An item with a path that cannot be evaluated is listed under not_captured and no file is written for it.",
            "schemas": "A tool's input_schema is its declared zod schema converted by zodschema.py, a port of the conversion in the bundle (zod toJSONSchema, draft 2020-12, plus the bundle's wrapper that recomputes `required`). Member order follows the library.",
            "environment_variables": "A CLAUDE_CODE_* style variable read through the bundle's environment object is treated as unset. The ones read are listed under assumed_unset_environment.",
            "feature_flags": "A remote flag read as getter(\"tengu_...\", default) evaluates to the default compiled into the bundle. The ones read are listed under feature_flag_defaults. A session whose account has a flag set differently gets different text.",
        },
        "limits": [
            "Tool descriptions are prompt() results. Hooks that plugins run on tool descriptions are not applied.",
            "input_schema is the declared schema. Upstream removes some properties when it builds a request (for example agent-team fields, and the `_host` field of remotely executed tools). That step is not applied.",
            "System prompt sections are captured one by one. The order is the order in the assembler's source; which sections a given session includes is decided at run time.",
            "Sections and tools that need session data (memory files, environment report, skills, MCP servers, output styles) are under not_captured.",
            "Only the linux-x64 build was read.",
        ],
        "assumed_unset_environment": sorted(capture.interp.env_vars),
        "feature_flag_defaults": dict(sorted(capture.interp.flags.items())),
        "items": capture.items,
        "not_captured": sorted(capture.not_captured, key=lambda m: (m["kind"], m["name"], json.dumps(m["located"], sort_keys=True))),
        "files": sorted(capture.files, key=lambda f: f["path"]),
    }
    (out / "manifest.json").write_bytes(dump(manifest))
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--version", default=PINNED)
    parser.add_argument("--work", type=Path, help="scratch directory for the tarballs (default: $JCODE_SCRATCH_DIR/capture or $TMPDIR)")
    parser.add_argument("--out", type=Path, help="output directory (default: prompts/claude-code/<version>)")
    args = parser.parse_args()
    if not re.fullmatch(r"\d+\.\d+\.\d+", args.version):
        print("capture failed: --version must be an exact x.y.z version", file=sys.stderr)
        return 1
    work = (args.work or Path(os.environ.get("JCODE_SCRATCH_DIR") or os.environ.get("TMPDIR") or "/tmp") / "capture").resolve()
    out = (args.out or REPO / "prompts" / "claude-code" / args.version).resolve()
    # The evaluator recurses once per nested JavaScript expression, so it runs
    # on a thread with a stack sized for jseval.MAX_CALL_DEPTH nested calls.
    sys.setrecursionlimit(200_000)
    threading.stack_size(1 << 30)
    outcome = {}

    def work_thread():
        try:
            outcome["manifest"] = run(args.version, work, out)
        except (CaptureError, bunfs.BundleError, outdir.InstallError) as e:
            outcome["error"] = e

    thread = threading.Thread(target=work_thread)
    thread.start()
    thread.join()
    if "manifest" not in outcome:
        print(f"capture failed: {outcome.get('error', 'the worker thread died')}", file=sys.stderr)
        return 1
    manifest = outcome["manifest"]
    items = manifest["items"]
    print(
        f"wrote {out}: {len(items.get('tools', {}))} tools, {len(items.get('system', {}))} system sections, "
        f"{len(items.get('interruption', []))} interruption texts, {len(manifest['not_captured'])} not captured"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
