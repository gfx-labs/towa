# /// script
# requires-python = ">=3.12"
# dependencies = []
# ///
"""Extract the tool definitions from a request recorded by
claude_code_record.py. Runs nothing: it reads a report that already exists.

    uv run tools/capture/claude_code_request.py \\
        --report "$JCODE_SCRATCH_DIR/capture/record-2.1.280-XXXX/report/report.json" \\
        --profile print-sdk.default \\
        --static prompts/claude-code/2.1.280 \\
        --out "$JCODE_SCRATCH_DIR/capture/profile-review"

Writes <out>/<profile>.<model>/tools/<Name>.json and manifest.json. The model
is the one named in the request. Each tool file holds the name, description
and input_schema the executable sent, in the member order it sent them.

Only the tools list is extracted. The system blocks and messages of the same
request hold the sandbox's paths, the host's kernel release and identifiers
generated for that one run, so they stay in the report.

A tool whose text holds anything hostscan.py recognises is not written, and is
listed in the manifest with the class of what was found.
"""

import argparse
import hashlib
import json
import re
import shutil
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import claude_code_sandbox as inside
import hostscan
import outdir

TOOL_MEMBERS = ["name", "description", "input_schema"]


class ExtractError(Exception):
    pass


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def dump(value) -> bytes:
    # The layout of the statically captured tool files, so the two compare.
    return (json.dumps(value, indent=2, ensure_ascii=False) + "\n").encode("utf-8")


def no_duplicates(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise ExtractError(f"duplicate member {key!r} in the recorded request")
        out[key] = value
    return out


def parse_body(text: str) -> dict:
    body = json.loads(text, object_pairs_hook=no_duplicates)
    # Parsing loses nothing: writing the value back compactly gives the bytes
    # the executable sent, so numbers and escapes are as upstream wrote them.
    if json.dumps(body, separators=(",", ":"), ensure_ascii=False) != text:
        raise ExtractError("the request body does not survive a JSON round trip; the tool files would not be what was sent")
    return body


def compare_with_static(tool: dict, static_root: Path, static_manifest: dict) -> dict:
    name = tool["name"]
    entries = static_manifest["items"]["tools"].get(name)
    if not entries:
        reason = next((g["reason"] for g in static_manifest["not_captured"] if g["kind"] == "tool" and g["name"] == name), None)
        return {"static": "none", "static_reason": reason}
    variants = []
    for entry in entries:
        other = json.loads((static_root / entry["file"]).read_text(encoding="utf-8"))
        variants.append(
            {"file": entry["file"], "description_equal": other["description"] == tool["description"], "input_schema_equal": other["input_schema"] == tool["input_schema"]}
        )
    equal = [v["file"] for v in variants if v["description_equal"] and v["input_schema_equal"]]
    return {"static": "equal" if equal else "differs", "equal_to": equal, "variants": variants}


def extract(report: dict, report_bytes: bytes, profile: str, static_root: Path, out_root: Path) -> Path:
    if not report.get("executable_was_started"):
        raise ExtractError("the report is from a probe: the executable was not started")
    recorded = [i for i in report["invocations"] if i["label"] == "record"]
    if len(recorded) != 1 or not recorded[0]["ok"] or len(recorded[0]["runs"]) != 1:
        raise ExtractError("expected one passing record invocation with one run")
    run = recorded[0]["runs"][0]
    # Scanned here with the current scanner. The scan stored in the report is
    # not relied on: it was made by whatever version recorded it.
    raw_scan = hostscan.scan_runs([run], inside.PLACEHOLDER_KEY)
    if raw_scan["blocking"]:
        raise ExtractError(f"the recording has blocking scan findings: {raw_scan['blocking']}")
    requests = [r for r in run["requests"] if r["method"] == "POST" and r["path"].split("?")[0] == "/v1/messages"]
    if not requests or len(requests) != len(run["requests"]):
        raise ExtractError(f"expected only POST /v1/messages requests, found {[(r['method'], r['path']) for r in run['requests']]}")
    if any("body" not in r for r in requests):
        raise ExtractError("a request body is not UTF-8")
    bodies = [r["body"] for r in requests]
    identical = all(b == bodies[0] for b in bodies)
    if not identical:
        raise ExtractError("the recorded requests differ from each other; which one to extract needs a decision")
    body = parse_body(bodies[0])
    model = body["model"]
    if not isinstance(model, str) or not re.fullmatch(r"[A-Za-z0-9._-]+", model):
        raise ExtractError(f"model {model!r} is not usable in a directory name")
    headers = {k.lower(): v for k, v in requests[0]["headers"]}

    static_manifest = json.loads((static_root / "manifest.json").read_text(encoding="utf-8"))
    static_exe = next(p["executable_sha256"] for p in static_manifest["packages"] if "executable_sha256" in p)
    if static_exe != report["executable_sha256"] or static_manifest["version"] != report["claude_code_version"]:
        raise ExtractError("the report and the static capture are of different executables")

    facts = hostscan.host_facts()
    written, refused, comparison = [], [], {}
    final = out_root / f"{profile}.{model}"
    out_root.mkdir(parents=True, exist_ok=True)
    out = Path(tempfile.mkdtemp(prefix=f".{final.name}.building-", dir=out_root))
    (out / "tools").mkdir(parents=True)
    names = [t.get("name") for t in body["tools"]]
    if len(set(names)) != len(names):
        raise ExtractError("two tools in the request have the same name")
    for tool in body["tools"]:
        if list(tool) != TOOL_MEMBERS:
            raise ExtractError(f"tool {tool.get('name')!r} has members {list(tool)}, expected exactly {TOOL_MEMBERS}")
        name = tool["name"]
        if not re.fullmatch(r"[A-Za-z0-9_.-]+", name):
            raise ExtractError(f"tool name {name!r} is not usable as a file name")
        data = dump(tool)
        found = hostscan.counts(hostscan.find(data.decode("utf-8"), facts, inside.PLACEHOLDER_KEY))
        if found:
            refused.append({"name": name, "found_counts_by_class": found})
            continue
        rel = f"tools/{name}.json"
        (out / rel).write_bytes(data)
        written.append({"path": rel, "sha256": sha256(data), "bytes": len(data)})
        comparison[name] = compare_with_static(tool, static_root, static_manifest)

    sent = set(names)
    manifest = {
        "upstream": static_manifest["upstream"],
        "version": report["claude_code_version"],
        "profile": f"{profile}.{model}",
        "captured_by": "tools/capture/claude_code_record.py, extracted by tools/capture/claude_code_request.py",
        "scope": "The tools list of one recorded request: name, description and input_schema of each tool, as sent. Nothing else of the request is here.",
        "what_this_profile_is": {
            "arguments": run["args"],
            "user_agent": headers.get("user-agent"),
            "model_in_request": model,
            "anthropic_beta": headers.get("anthropic-beta"),
            "note": "Print mode with no settings, no login and an empty working directory. The model is the executable's default; it was not chosen with --model. Other modes, models, settings and accounts send other tool lists and descriptions, and none of those was recorded.",
        },
        "provenance": {
            "platform_tarball_integrity": report["platform_tarball_integrity"],
            "executable_sha256": report["executable_sha256"],
            "sandbox_script_sha256": report["sandbox_script_sha256"],
            "sandbox_runtime_manifest_sha256": report["runtime_manifest_sha256"],
            "report_sha256": sha256(report_bytes),
            "requests_recorded": len(run["requests"]),
            "request_bodies_identical": identical,
            "request_body_sha256": sha256(bodies[0].encode("utf-8")),
            "request_body_bytes": len(bodies[0].encode("utf-8")),
            "executable_exit_code": run.get("exit_code"),
            "model_side": "None. The recording server answered every request with HTTP 400, so no model reply exists and no tool was called.",
            "raw_request": "Not stored in the repository. It holds the sandbox's paths, the capture host's kernel release and identifiers generated for the run.",
            "raw_request_scan": {"blocking": raw_scan["blocking"], "noted_counts_by_class": raw_scan["noted_counts_by_class"]},
        },
        "tool_file_layout": "JSON, two-space indent, members in the order sent, non-ASCII characters unescaped, one trailing newline. The request body was checked to survive a JSON round trip byte for byte before extraction.",
        "tools_in_request": names,
        "files": written,
        "not_written": refused,
        "host_scan": {
            "classes_refused": hostscan.CLASSES,
            "checks_not_made": hostscan.skipped_checks(facts),
            "result": "Every written file has no match in any class." if not refused else "See not_written.",
        },
        "compared_with_static_capture": {
            "static_manifest_sha256": sha256((static_root / "manifest.json").read_bytes()),
            "per_tool": comparison,
            "static_tools_not_in_this_request": sorted(set(static_manifest["items"]["tools"]) - sent),
        },
    }
    (out / "manifest.json").write_bytes(dump(manifest))
    try:
        outdir.install(out, final)
    finally:
        shutil.rmtree(out)
    return final


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--report", type=Path, required=True, help="report.json written by claude_code_record.py")
    parser.add_argument("--profile", required=True, help="name for how the executable was run, for example print-sdk.default; the model is appended")
    parser.add_argument("--static", type=Path, required=True, help="the static capture of the same version, for comparison")
    parser.add_argument("--out", type=Path, required=True, help="directory to write <profile>.<model>/ into")
    opts = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9._-]+", opts.profile):
        parser.error("--profile may hold letters, digits, dot, underscore and hyphen")
    try:
        report_bytes = opts.report.read_bytes()
        out = extract(json.loads(report_bytes), report_bytes, opts.profile, opts.static, opts.out)
    except (ExtractError, outdir.InstallError, KeyError, StopIteration) as e:
        print(f"extraction failed: {type(e).__name__}: {e}", file=sys.stderr)
        return 1
    manifest = json.loads((out / "manifest.json").read_text(encoding="utf-8"))
    print(f"wrote {out}: {len(manifest['files'])} tools, {len(manifest['not_written'])} not written")
    return 0 if not manifest["not_written"] else 1


if __name__ == "__main__":
    sys.exit(main())
