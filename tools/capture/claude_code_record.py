# /// script
# requires-python = ">=3.12"
# dependencies = ["tree-sitter==0.25.2", "tree-sitter-javascript==0.25.0", "regex==2026.11.9"]  # as claude_code.py, imported below
# ///
"""Record the requests the pinned Claude Code executable sends, by running it
inside a bubblewrap sandbox against a recording server in the same sandbox.

    uv run tools/capture/claude_code_record.py --probe-only
    uv run tools/capture/claude_code_record.py --timeout 120 --run default -- -p hi
    uv run tools/capture/claude_code_record.py --write-runtime-manifest

The sandbox root is built from nothing and holds four things:

    /usr    read-only. A Python runtime copied file by file from this host,
            each file listed with its sha256 in sandbox_runtime.json.
    /input  read-only. The executable, claude_code_sandbox.py and plan.json.
    /work   writable and empty. The only place the sandbox can write.
    /dev, /proc, /tmp  fresh.

No network but its own loopback, no /home, /etc, /run, /nix or /mnt, and an
environment of three variables. claude_code_sandbox.py compares all of that
with exact expected values before the executable is started, and this script
repeats the comparison on the values it reads back.

Everything under /work is written by the sandbox and is read here as untrusted
data: opened without following a symlink, required to be a regular file, never
written to. The report is written to a directory the sandbox never saw.

A run that starts the executable is preceded by a probe in its own sandbox,
where only claude_code_sandbox.py runs. Nothing is written to prompts/.
"""

import argparse
import base64
import getpass
import hashlib
import json
import os
import re
import shutil
import socket
import stat
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import claude_code
import claude_code_sandbox as inside

BWRAP = "/usr/bin/bwrap"
SANDBOX_SCRIPT = HERE / "claude_code_sandbox.py"
RUNTIME_MANIFEST = HERE / "sandbox_runtime.json"
HOST_PYTHON = "/usr/bin/python3"
HOST_LIBS = "/usr/lib/x86_64-linux-gnu"
# The loader and the shared libraries `ldd` lists for the interpreter (libc,
# libm, libz, libexpat) and for the Claude Code executable (libc, libm, librt,
# libpthread, libdl).
HOST_LIBRARIES = ["ld-linux-x86-64.so.2", "libc.so.6", "libm.so.6", "libz.so.1", "libexpat.so.1", "librt.so.1", "libpthread.so.0", "libdl.so.2"]
LOADER_LINK = {"path": "lib64/ld-linux-x86-64.so.2", "symlink": "../lib/x86_64-linux-gnu/ld-linux-x86-64.so.2"}
# Empty. The interpreter looks for it at startup and warns on stderr without it.
DYNLOAD_DIRECTORY = {"path": "lib/python3.13/lib-dynload", "directory": True}

# What a recorded request may not contain. Checked on every header and body.
CREDENTIAL_HEADERS = ("authorization", "x-api-key", "cookie", "proxy-authorization")


class RecordError(Exception):
    pass


def sha256_file(path) -> str:
    with open(path, "rb") as f:
        return hashlib.file_digest(f, "sha256").hexdigest()


# -- runtime -----------------------------------------------------------


def build_runtime_manifest() -> list:
    """The files of the sandbox's /usr, from this host: the interpreter, the
    libraries above, and the standard library files the sandbox script loads."""
    listed = subprocess.run(
        [HOST_PYTHON, *inside.PYTHON_ARGS, str(SANDBOX_SCRIPT), "--list-modules"], env={}, capture_output=True, check=True, stdin=subprocess.DEVNULL
    )
    entries = [{"path": "bin/python3", "source": os.path.realpath(HOST_PYTHON)}]
    entries += [{"path": f"lib/x86_64-linux-gnu/{name}", "source": os.path.realpath(f"{HOST_LIBS}/{name}")} for name in HOST_LIBRARIES]
    for source in json.loads(listed.stdout):
        if not source.endswith(".py"):
            raise RecordError(f"{source} is not a .py file: a compiled module needs its libraries reviewed before it is added")
        entries.append({"path": os.path.relpath(source, "/usr"), "source": source})
    for entry in entries:
        entry["sha256"] = sha256_file(entry["source"])
    return [*sorted(entries, key=lambda e: e["path"]), DYNLOAD_DIRECTORY, LOADER_LINK]


def load_runtime_manifest() -> list:
    if not RUNTIME_MANIFEST.is_file():
        raise RecordError(f"{RUNTIME_MANIFEST.name} is missing: run --write-runtime-manifest and review it")
    manifest = json.loads(RUNTIME_MANIFEST.read_text())
    for entry in manifest["files"]:
        path = Path(entry["path"])
        if path.is_absolute() or ".." in path.parts:
            raise RecordError(f"runtime manifest path {entry['path']} leaves /usr")
    return manifest["files"]


def install_runtime(entries: list, usr: Path) -> dict:
    """Copy the listed files into usr and return the inventory the sandbox
    must report for /usr. A host file that no longer has the listed sha256 is
    an error: the manifest is reviewed, the host's files are not."""
    for entry in entries:
        target = usr / entry["path"]
        target.parent.mkdir(parents=True, exist_ok=True)
        if "symlink" in entry:
            target.symlink_to(entry["symlink"])
            continue
        if entry.get("directory"):
            target.mkdir()
            continue
        data = Path(entry["source"]).read_bytes()
        if hashlib.sha256(data).hexdigest() != entry["sha256"]:
            raise RecordError(f"{entry['source']} differs from {RUNTIME_MANIFEST.name}: review the change, then run --write-runtime-manifest")
        target.write_bytes(data)
        target.chmod(0o555 if entry["path"].startswith("bin/") or ".so" in target.name else 0o444)
    return inside.inventory(str(usr))


# -- sandbox -----------------------------------------------------------


def sandbox_command(session: Path, work: Path) -> list:
    command = [BWRAP, "--unshare-all", "--die-with-parent", "--new-session", "--hostname", inside.HOSTNAME, "--clearenv"]
    for name, value in inside.ENVIRONMENT.items():
        if name != "PWD":  # set by --chdir
            command += ["--setenv", name, value]
    command += ["--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp"]
    command += ["--ro-bind", str(session / "usr"), "/usr", "--ro-bind", str(session / "input"), inside.INPUT]
    for link, target in inside.ROOT_SYMLINKS.items():
        command += ["--symlink", target, link]
    command += ["--bind", str(work), inside.WORK, "--chdir", inside.WORK]
    return [*command, "/usr/bin/python3", *inside.PYTHON_ARGS, f"{inside.INPUT}/sandbox.py"]


def read_untrusted(directory: Path, name: str, limit: int) -> bytes:
    """A file the sandbox wrote. No symlink is followed at the directory or the
    file, and anything but a singly linked regular file is refused."""
    directory_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        try:
            # O_NONBLOCK: opening a FIFO planted under this name must not hang.
            fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory_fd)
        except OSError as e:
            raise RecordError(f"{name} in the sandbox work directory cannot be opened as a plain file: {e.strerror}") from e
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                raise RecordError(f"{name} in the sandbox work directory is not a singly linked regular file")
            if info.st_size > limit:
                raise RecordError(f"{name} is {info.st_size} bytes, over --max-result-bytes {limit}")
            with os.fdopen(os.dup(fd), "rb") as f:
                data = f.read(limit + 1)
            if len(data) > limit:
                raise RecordError(f"{name} grew past --max-result-bytes {limit} while it was read")
            return data
        finally:
            os.close(fd)
    finally:
        os.close(directory_fd)


def invoke(session: Path, label: str, runs: list, timeout, expected: dict, limit: int) -> dict:
    """One sandbox: the probe, then the given runs of the executable. Returns
    what the sandbox reported, with this script's own verdict on the probe."""
    work = session / f"work-{label}"
    work.mkdir()
    plan = {"runs": runs, "timeout_seconds": timeout, "expected_usr": expected["usr"], "expected_input": expected["input"]}
    plan_path = session / "input" / "plan.json"
    plan_path.write_text(json.dumps(plan))
    command = sandbox_command(session, work)
    # The pid namespace ends with this process, so nothing in the sandbox is
    # still running, or still writing, when the result is read.
    done = subprocess.run(command, env={}, stdin=subprocess.DEVNULL, capture_output=True, check=False)
    out = {
        "label": label,
        "command": command,
        "exit_code": done.returncode,
        "stderr": done.stderr.decode("utf-8", "replace"),
        "stdout": done.stdout.decode("utf-8", "replace"),
    }
    # The read-only binds are checked from outside as well.
    out["host_view_after"] = {
        "usr_unchanged": inside.inventory(str(session / "usr")) == expected["usr"],
        "input_unchanged": {k: v for k, v in inside.inventory(str(session / "input")).items() if k != "plan.json"} == expected["input"],
    }
    try:
        result = json.loads(read_untrusted(work, "result.json", limit))
        observed = result["probe"]["observed"]
        checks = inside.evaluate(observed, plan)
        reported_runs = result["runs"]
        if not isinstance(reported_runs, list) or [r["label"] for r in reported_runs] != [r["label"] for r in runs][: len(reported_runs)]:
            raise RecordError("the runs in the result are not the runs that were planned")
    except (RecordError, ValueError, KeyError, TypeError, AttributeError) as e:
        out["result_error"] = f"{type(e).__name__}: {e}"
        out["ok"] = False
        return out
    out["probe_observed"] = observed
    out["probe_checks_by_host"] = checks
    out["probe_ok_claimed_by_sandbox"] = result["probe"].get("ok")
    out["runs"] = reported_runs
    out["environment_given_to_executable"] = result.get("environment_given_to_executable")
    out["ok"] = all(checks.values()) and all(out["host_view_after"].values()) and done.returncode == 0 and len(reported_runs) == len(runs)
    return out


def scan(runs: list) -> list:
    """Findings that block publication: a credential that is not the
    placeholder, or a trace of this host in what the executable sent or printed."""
    host_marks = {"home directory path": "/home/", "host name": socket.gethostname(), "user name": getpass.getuser()}
    key = inside.PLACEHOLDER_KEY
    findings = []

    def check_text(where: str, text: str):
        for token in set(re.findall(r"sk-ant-[A-Za-z0-9_-]+", text)) - {key}:
            findings.append(f"{where}: key-shaped text other than the placeholder ({len(token)} characters)")
        for label, mark in host_marks.items():
            if mark and mark in text:
                findings.append(f"{where}: contains the {label}")

    for run in runs:
        for stream in ("stdout", "stderr"):
            check_text(f"run {run['label']} {stream}", str(run.get(stream, "")))
        for index, request in enumerate(run["requests"]):
            where = f"run {run['label']} request {index} ({request['method']} {request['path']})"
            for name, value in request["headers"]:
                if name.lower() in CREDENTIAL_HEADERS and value not in (key, f"Bearer {key}"):
                    findings.append(f"{where}: header {name} is not the placeholder")
            if "body_base64" in request:
                findings.append(f"{where}: the body is not UTF-8 and was not scanned")
                body = base64.b64decode(request["body_base64"]).decode("utf-8", "replace")
            else:
                body = request["body"]
            check_text(where, request["path"] + "\n" + "\n".join(f"{k}: {v}" for k, v in request["headers"]) + "\n" + body)
    return findings


def write_report(report_dir: Path, report: dict) -> Path:
    """Written beside the session's sandbox directories, in one the sandbox was
    never given, through a temporary file so a reader sees all of it or none."""
    report_dir.mkdir(exist_ok=True)
    fd, temporary = tempfile.mkstemp(dir=report_dir, prefix=".report-")
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        json.dump(report, f, indent=2)
    final = report_dir / "report.json"
    os.replace(temporary, final)
    return final


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--version", default=claude_code.PINNED)
    parser.add_argument("--work", type=Path, default=None, help="where tarballs and sessions are kept (default $JCODE_SCRATCH_DIR/capture)")
    parser.add_argument("--probe-only", action="store_true", help="check the sandbox and do not run the executable")
    parser.add_argument("--write-runtime-manifest", action="store_true", help=f"rewrite {RUNTIME_MANIFEST.name} from this host's files, for review")
    parser.add_argument("--run", action="append", default=[], metavar="LABEL", help="label for a run; give once per argument list")
    parser.add_argument("--timeout", type=int, default=None, help="seconds allowed for each run of the executable")
    parser.add_argument(
        "--max-result-bytes", type=int, default=64 * 1024 * 1024, help="largest result file accepted from the sandbox; a larger one is an error"
    )
    parser.add_argument("args", nargs="*", help="argument lists for the executable, separated by ---")
    opts = parser.parse_args()

    if opts.write_runtime_manifest:
        files = build_runtime_manifest()
        RUNTIME_MANIFEST.write_text(json.dumps({"host_python": sys.version.split()[0], "files": files}, indent=2) + "\n")
        print(f"wrote {RUNTIME_MANIFEST} with {len(files)} entries")
        return 0

    scratch = os.environ.get("JCODE_SCRATCH_DIR")
    work = opts.work or (Path(scratch) / "capture" if scratch else None)
    if work is None:
        parser.error("set JCODE_SCRATCH_DIR or pass --work")
    arg_lists, current = [], []
    for a in opts.args:
        if a == "---":
            arg_lists.append(current)
            current = []
        else:
            current.append(a)
    if current or (opts.args and opts.args[-1] == "---"):
        arg_lists.append(current)
    runs = []
    if not opts.probe_only:
        if not opts.run or len(opts.run) != len(arg_lists):
            parser.error("give one --run LABEL per argument list")
        if opts.timeout is None:
            parser.error("--timeout is required when the executable is run")
        runs = [{"label": label, "args": args} for label, args in zip(opts.run, arg_lists)]

    try:
        platform = claude_code.fetch(claude_code.PLATFORM, opts.version, work).read_bytes()
        executable = claude_code.member(platform, claude_code.EXECUTABLE)
        work.mkdir(parents=True, exist_ok=True)
        session = Path(tempfile.mkdtemp(prefix=f"record-{opts.version}-", dir=work))
        expected = {"usr": install_runtime(load_runtime_manifest(), session / "usr")}
        (session / "input").mkdir()
        (session / "input" / "claude").write_bytes(executable)
        (session / "input" / "claude").chmod(0o555)
        shutil.copyfile(SANDBOX_SCRIPT, session / "input" / "sandbox.py")
        expected["input"] = inside.inventory(str(session / "input"))
    except (RecordError, claude_code.CaptureError) as e:
        print(f"record failed: {e}", file=sys.stderr)
        return 1

    report = {
        "claude_code_version": opts.version,
        "platform_tarball_integrity": "sha512-" + base64.b64encode(hashlib.sha512(platform).digest()).decode(),
        "executable_sha256": hashlib.sha256(executable).hexdigest(),
        "sandbox_script_sha256": sha256_file(SANDBOX_SCRIPT),
        "runtime_manifest_sha256": sha256_file(RUNTIME_MANIFEST),
        "executable_was_started": False,
        "expected": {
            "usr_inventory": expected["usr"],
            "input_inventory": expected["input"],
            "environment": inside.ENVIRONMENT,
            "hostname": inside.HOSTNAME,
            "root_listing": inside.ROOT_ENTRIES,
            "root_symlinks": inside.ROOT_SYMLINKS,
            "dev_listing": inside.DEV_ENTRIES,
            "sys_path": inside.SYS_PATH,
        },
        "invocations": [],
        "scan_findings": [],
    }
    # The probe runs alone first. Its result comes from a sandbox in which only
    # claude_code_sandbox.py ran, so nothing else could have written it.
    probe = invoke(session, "probe", [], None, expected, opts.max_result_bytes)
    report["invocations"].append(probe)
    ok = probe["ok"]
    if ok and runs:
        report["executable_was_started"] = True
        recorded = invoke(session, "record", runs, opts.timeout, expected, opts.max_result_bytes)
        report["invocations"].append(recorded)
        ok = recorded["ok"]
        if "runs" in recorded:
            try:
                report["scan_findings"] = scan(recorded["runs"])
            except (KeyError, TypeError, ValueError) as e:
                report["scan_findings"] = [f"the recorded runs are malformed and were not scanned: {type(e).__name__}: {e}"]
    path = write_report(session / "report", report)

    for invocation in report["invocations"]:
        print(f"{invocation['label']}: {'ok' if invocation['ok'] else 'FAILED'} (sandbox exit {invocation['exit_code']})")
        if "result_error" in invocation:
            print(f"  result: {invocation['result_error']}")
        for name, passed in invocation.get("probe_checks_by_host", {}).items():
            if not passed:
                print(f"  check failed: {name}")
        for name, passed in invocation["host_view_after"].items():
            if not passed:
                print(f"  check failed: {name}")
        for run in invocation.get("runs", []):
            seen = [f"{r['method']} {r['path']}" for r in run["requests"]]
            print(f"  run {run['label']}: exit {run.get('exit_code', 'timeout')}, {len(seen)} requests: {seen}")
    for finding in report["scan_findings"]:
        print(f"SCAN: {finding}")
    print(f"executable started: {report['executable_was_started']}")
    print(f"report: {path}")
    return 0 if ok and not report["scan_findings"] else 1


if __name__ == "__main__":
    sys.exit(main())
