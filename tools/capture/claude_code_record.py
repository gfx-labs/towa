# /// script
# requires-python = ">=3.12"
# dependencies = ["tree-sitter==0.25.2", "tree-sitter-javascript==0.25.0", "regex==2026.11.9"]  # as claude_code.py, imported below
# ///
"""Record the requests the pinned Claude Code executable sends, by running it
inside a bubblewrap sandbox against a recording server in the same sandbox.

    uv run tools/capture/claude_code_record.py --probe-only
    uv run tools/capture/claude_code_record.py --run default -- -p hi

The sandbox root is built from nothing: the system Python, the shared libraries
it and the executable load, and one writable directory made for the run, at
/work. No /home, /etc, /run, /nix or /mnt, no network except its own loopback,
and a cleared environment.
claude_code_sandbox.py checks all of that from inside before the executable is
started, and the executable is not started when a check fails.

The run directory is left in place for inspection. Nothing is written to
prompts/ by this script.
"""

import argparse
import getpass
import hashlib
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import claude_code

BWRAP = "/usr/bin/bwrap"
PYTHON = "/usr/bin/python3"
PYTHON_STDLIB = "/usr/lib/python3.13"
LIBS = "/usr/lib/x86_64-linux-gnu"
# The sandbox root is built from nothing. These files, the Python standard
# library and the run directory are all it holds: the interpreter, the loader,
# and the shared libraries `ldd` lists for the interpreter and the executable.
RUNTIME_FILES = [
    PYTHON,
    "/usr/lib64/ld-linux-x86-64.so.2",
    f"{LIBS}/ld-linux-x86-64.so.2",
    f"{LIBS}/libc.so.6",
    f"{LIBS}/libm.so.6",
    f"{LIBS}/libz.so.1",
    f"{LIBS}/libexpat.so.1",
    f"{LIBS}/librt.so.1",
    f"{LIBS}/libpthread.so.0",
    f"{LIBS}/libdl.so.2",
]

# What a recorded request may not contain. Checked on every header and body.
PLACEHOLDER_KEY = "sk-ant-placeholder-not-a-real-key"
CREDENTIAL_HEADERS = ("authorization", "x-api-key", "cookie", "proxy-authorization")


def scan(result: dict) -> list:
    """Findings that block publication: a credential that is not the
    placeholder, or a trace of the host in what the executable sent."""
    host_marks = {"home directory path": "/home/", "host name": socket.gethostname(), "user name path": f"/{getpass.getuser()}/"}
    findings = []
    for run in result["runs"]:
        for index, request in enumerate(run["requests"]):
            where = f"run {run['label']} request {index} ({request['method']} {request['path']})"
            for name, value in request["headers"]:
                if name.lower() in CREDENTIAL_HEADERS and value not in (PLACEHOLDER_KEY, f"Bearer {PLACEHOLDER_KEY}"):
                    findings.append(f"{where}: header {name} is not the placeholder")
            text = request["path"] + "\n" + "\n".join(f"{k}: {v}" for k, v in request["headers"]) + "\n" + request["body"]
            for token in set(re.findall(r"sk-ant-[A-Za-z0-9_-]+", text)) - {PLACEHOLDER_KEY}:
                findings.append(f"{where}: key-shaped text other than the placeholder ({len(token)} characters)")
            for label, mark in host_marks.items():
                if mark and mark in text:
                    findings.append(f"{where}: contains the {label}")
    return findings


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--version", default=claude_code.PINNED)
    parser.add_argument("--work", type=Path, default=None, help="where tarballs are kept (default $JCODE_SCRATCH_DIR/capture)")
    parser.add_argument("--probe-only", action="store_true", help="check the sandbox and do not run the executable")
    parser.add_argument("--run", action="append", default=[], metavar="LABEL", help="label for a run; give once per argument list")
    parser.add_argument("--timeout", type=int, required=False, default=None, help="seconds allowed for each run of the executable")
    parser.add_argument("args", nargs="*", help="argument lists for the executable, separated by ---")
    opts = parser.parse_args()

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
    if opts.probe_only:
        runs = []
    else:
        if not opts.run or len(opts.run) != len(arg_lists):
            parser.error("give one --run LABEL per argument list")
        if opts.timeout is None:
            parser.error("--timeout is required when the executable is run")
        runs = [{"label": label, "args": args} for label, args in zip(opts.run, arg_lists)]

    platform = claude_code.fetch(claude_code.PLATFORM, opts.version, work).read_bytes()
    executable = claude_code.member(platform, claude_code.EXECUTABLE)

    work.mkdir(parents=True, exist_ok=True)
    run_dir = Path(tempfile.mkdtemp(prefix=f"record-{opts.version}-", dir=work))
    (run_dir / "claude").write_bytes(executable)
    (run_dir / "claude").chmod(0o755)
    shutil.copy(HERE / "claude_code_sandbox.py", run_dir / "sandbox.py")
    (run_dir / "plan.json").write_text(json.dumps({"runs": runs, "timeout_seconds": opts.timeout}))

    command = [BWRAP, "--unshare-all", "--die-with-parent", "--new-session", "--hostname", "sandbox", "--clearenv",
               "--setenv", "PATH", "/usr/bin", "--setenv", "HOME", "/work",
               "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp"]  # fmt: skip
    for path in RUNTIME_FILES:
        real = os.path.realpath(path)
        if not os.path.isfile(real):
            print(f"runtime file {path} is missing on this host", file=sys.stderr)
            return 1
        command += ["--ro-bind", real, path]
    command += ["--ro-bind", PYTHON_STDLIB, PYTHON_STDLIB]
    command += ["--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64"]
    command += ["--bind", str(run_dir), "/work", "--chdir", "/work", PYTHON, "-I", "/work/sandbox.py", "/work"]
    # The exit code is reported with the probe result below, not raised.
    done = subprocess.run(command, env={}, stdin=subprocess.DEVNULL, check=False)
    result_path = run_dir / "result.json"
    if not result_path.exists():
        print(f"the sandbox wrote no result (exit {done.returncode}); run directory {run_dir}", file=sys.stderr)
        return 1
    result = json.loads(result_path.read_text())
    result["sandbox_command"] = [c if c != str(run_dir) else "<run dir>" for c in command]
    result["executable_sha256"] = hashlib.sha256(executable).hexdigest()
    result["scan_findings"] = scan(result)
    result_path.write_text(json.dumps(result, indent=2))
    print(json.dumps(result["probe"], indent=2))
    for run in result["runs"]:
        seen = [f"{r['method']} {r['path']}" for r in run["requests"]]
        print(f"run {run['label']}: exit {run.get('exit_code', 'timeout')}, {len(seen)} requests: {seen}")
    for finding in result["scan_findings"]:
        print(f"SCAN: {finding}")
    print(f"run directory: {run_dir}")
    return 0 if result["probe"]["ok"] and done.returncode == 0 and not result["scan_findings"] else 1


if __name__ == "__main__":
    sys.exit(main())
