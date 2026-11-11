"""Runs inside the bubblewrap sandbox started by claude_code_record.py.

    /usr/bin/python3 -I -S -B /input/sandbox.py

The sandbox root holds a minimal Python runtime under /usr, read-only inputs
under /input (the executable, this script, plan.json) and an empty writable
/work. This script checks the sandbox against exact expected values, tests its
own recording server, and only then runs the Claude Code executable against
that server. The result goes to /work/result.json.

The server answers every request with HTTP 400, so no model output exists
anywhere in the result. Only the request the CLI builds is recorded.
"""

import sys

# Optional modules the standard library imports when it finds them. They are
# not in the sandbox runtime. Blocking them here makes the module list taken
# on the host (--list-modules) the same as what the sandbox loads.
BLOCKED_MODULES = ("ssl", "_ssl", "_hashlib", "bz2", "_bz2", "lzma", "_lzma", "zlib", "pwd", "grp", "readline", "_ctypes", "ctypes")
if __name__ == "__main__":
    for _name in BLOCKED_MODULES:
        sys.modules[_name] = None

import base64
import encodings.idna  # noqa: F401  socket.getaddrinfo loads it for a host name
import hashlib
import http.client
import http.server
import json
import os
import socket
import stat
import subprocess
import threading
import typing

PLACEHOLDER_KEY = "sk-ant-placeholder-not-a-real-key"
INPUT = "/input"
WORK = "/work"
STDLIB = "/usr/lib/python3.13"

# Exact expected state. A difference in any of these fails the probe.
ROOT_ENTRIES = ["dev", "input", "lib", "lib64", "proc", "tmp", "usr", "work"]
ROOT_SYMLINKS = {"/lib": "usr/lib", "/lib64": "usr/lib64"}
# bubblewrap adds /dev/console only when its stdout is a terminal. The host
# script gives it a pipe, so a console here means the sandbox holds a terminal.
DEV_ENTRIES = ["core", "fd", "full", "null", "ptmx", "pts", "random", "shm", "stderr", "stdin", "stdout", "tty", "urandom", "zero"]
INPUT_ENTRIES = ["claude", "plan.json", "sandbox.py"]
ENVIRONMENT = {"HOME": WORK, "PATH": "/usr/bin", "PWD": WORK}
HOSTNAME = "sandbox"
SYS_PATH = ["/usr/lib/python313.zip", STDLIB, f"{STDLIB}/lib-dynload"]
PYTHON_ARGS = ["-I", "-S", "-B"]
OUTBOUND = (("1.1.1.1", 443), ("127.0.0.53", 53))


def inventory(top: str) -> dict:
    """Every entry under top: sha256 for a file, "dir" for a directory,
    "symlink -> target" for a link, the file type for anything else."""
    found = {}
    stack = [top]
    while stack:
        base = stack.pop()
        for name in sorted(os.listdir(base)):
            path = os.path.join(base, name)
            rel = os.path.relpath(path, top)
            mode = os.lstat(path).st_mode
            if stat.S_ISLNK(mode):
                found[rel] = f"symlink -> {os.readlink(path)}"
            elif stat.S_ISDIR(mode):
                found[rel] = "dir"
                stack.append(path)
            elif stat.S_ISREG(mode):
                with open(path, "rb") as f:
                    found[rel] = hashlib.file_digest(f, "sha256").hexdigest()
            else:
                found[rel] = f"other mode {stat.S_IFMT(mode):o}"
    return dict(sorted(found.items()))


def writable(directory: str) -> bool:
    try:
        fd = os.open(os.path.join(directory, ".probe-write"), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    except OSError:
        return False
    os.close(fd)
    os.unlink(os.path.join(directory, ".probe-write"))
    return True


class Recorder(http.server.BaseHTTPRequestHandler):
    # Shared by every handler instance on purpose: one list for the server.
    records: typing.ClassVar[list] = []
    lock = threading.Lock()

    def handle_any(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        record = {"method": self.command, "path": self.path, "headers": [[k, v] for k, v in self.headers.items()]}
        try:
            record["body"] = body.decode("utf-8")
        except UnicodeDecodeError:
            record["body_base64"] = base64.b64encode(body).decode("ascii")
        with self.lock:
            self.records.append(record)
        answer = json.dumps({"type": "error", "error": {"type": "invalid_request_error", "message": "recording server: no model here"}}).encode()
        self.send_response(400)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(answer)))
        self.end_headers()
        self.wfile.write(answer)

    do_GET = do_POST = do_PUT = do_HEAD = do_DELETE = do_PATCH = do_OPTIONS = handle_any

    def log_message(self, *args):
        pass


def start_server():
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Recorder)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def self_test(server) -> dict:
    """The recording path and the child-process path, exercised without the
    executable: one request to the server, one child interpreter."""
    marker = '{"self_test": true}'
    connection = http.client.HTTPConnection("127.0.0.1", server.server_address[1], timeout=10)
    connection.request("POST", "/v1/self-test", body=marker, headers={"x-api-key": PLACEHOLDER_KEY, "Content-Type": "application/json"})
    response = connection.getresponse()
    answer = response.read()
    connection.close()
    with Recorder.lock:
        records = list(Recorder.records)
        Recorder.records.clear()
    # The script itself, not `-c`: that would need a module only `-c` loads.
    child = subprocess.run([sys.executable, *PYTHON_ARGS, __file__, "--child"], capture_output=True, timeout=30, check=False, stdin=subprocess.DEVNULL)
    recorded = len(records) == 1 and records[0]["method"] == "POST" and records[0]["path"] == "/v1/self-test" and records[0].get("body") == marker
    return {
        "ok": recorded and response.status == 400 and b"recording server" in answer and child.returncode == 0 and child.stdout == b"child ran\n",
        "server_status": response.status,
        "requests_recorded": len(records),
        "child_exit_code": child.returncode,
        "child_stderr": child.stderr.decode("utf-8", "replace"),
    }


def probe(plan: dict, server) -> dict:
    """The sandbox compared with exact expected values. ok is every check."""
    links = sorted(name for _, name in socket.if_nameindex())
    connects = {}
    for host, port in OUTBOUND:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(3)
        try:
            s.connect((host, port))
            connects[f"{host}:{port}"] = "connected"
        except OSError as e:
            connects[f"{host}:{port}"] = f"failed: {type(e).__name__} errno {e.errno}"
        finally:
            s.close()
    try:
        socket.getaddrinfo("api.anthropic.com", 443)
        dns = "resolved"
    except OSError as e:
        dns = f"failed: {type(e).__name__}"
    root = sorted(os.listdir("/"))
    symlinks = {p: (os.readlink(p) if os.path.islink(p) else "not a symlink") for p in ROOT_SYMLINKS}
    usr = inventory("/usr")
    inputs = inventory(INPUT)
    modules = sorted({m.__file__ for m in list(sys.modules.values()) if getattr(m, "__file__", None)})
    tested = self_test(server)
    observed = {
        "network_links": links,
        "outbound_connects": connects,
        "dns_lookup_api_anthropic_com": dns,
        "root_listing": root,
        "root_symlinks": symlinks,
        "dev_listing": sorted(os.listdir("/dev")),
        "usr_inventory": usr,
        "input_inventory": inputs,
        "work_listing_at_start": sorted(os.listdir(WORK)),
        "tmp_listing_at_start": sorted(os.listdir("/tmp")),
        "environment": dict(os.environ),
        "hostname": socket.gethostname(),
        "cwd": os.getcwd(),
        "uid": os.getuid(),
        "sys_path": list(sys.path),
        "sys_flags": {"isolated": sys.flags.isolated, "no_site": sys.flags.no_site, "dont_write_bytecode": sys.flags.dont_write_bytecode},
        "module_files_loaded": modules,
        "site_modules_loaded": sorted(name for name in ("site", "sitecustomize", "usercustomize") if name in sys.modules),
        "usr_writable": writable("/usr") or writable(STDLIB),
        "input_writable": writable(INPUT),
        "self_test": tested,
    }
    checks = evaluate(observed, plan)
    return {"ok": all(checks.values()), "checks": checks, "observed": observed}


def evaluate(observed: dict, plan: dict) -> dict:
    """Each check as true or false. The host runs this again on the observed
    values it reads back, so it does not depend on the sandbox's own verdict."""
    inputs = observed["input_inventory"]
    return {
        "network_links_are_only_lo": observed["network_links"] == ["lo"],
        "outbound_connects_fail": sorted(observed["outbound_connects"]) == sorted(f"{h}:{p}" for h, p in OUTBOUND)
        and all(v.startswith("failed") for v in observed["outbound_connects"].values()),
        "dns_lookup_fails": observed["dns_lookup_api_anthropic_com"].startswith("failed"),
        "root_listing_exact": observed["root_listing"] == ROOT_ENTRIES,
        "root_symlinks_exact": observed["root_symlinks"] == ROOT_SYMLINKS,
        "dev_listing_exact": observed["dev_listing"] == DEV_ENTRIES,
        "usr_inventory_exact": observed["usr_inventory"] == plan["expected_usr"],
        "input_listing_exact": sorted(inputs) == INPUT_ENTRIES,
        "input_hashes_exact": {k: v for k, v in inputs.items() if k != "plan.json"} == plan["expected_input"],
        "usr_is_read_only": observed["usr_writable"] is False,
        "input_is_read_only": observed["input_writable"] is False,
        "work_is_empty_at_start": observed["work_listing_at_start"] == [],
        "tmp_is_empty_at_start": observed["tmp_listing_at_start"] == [],
        "environment_exact": observed["environment"] == ENVIRONMENT,
        "hostname_exact": observed["hostname"] == HOSTNAME,
        "cwd_exact": observed["cwd"] == WORK,
        "sys_path_exact": observed["sys_path"] == SYS_PATH,
        "interpreter_isolated_without_site": observed["sys_flags"] == {"isolated": 1, "no_site": 1, "dont_write_bytecode": 1},
        "site_not_loaded": observed["site_modules_loaded"] == [],
        "modules_only_from_runtime_or_input": all(f == f"{INPUT}/sandbox.py" or f.startswith(STDLIB + "/") for f in observed["module_files_loaded"]),
        "self_test_passes": observed["self_test"]["ok"] is True,
    }


def executable_environment(port: int) -> dict:
    home = f"{WORK}/home"
    return {
        "PATH": "/usr/bin",
        "HOME": home,
        "XDG_CONFIG_HOME": f"{home}/xdg-config",
        "XDG_DATA_HOME": f"{home}/xdg-data",
        "XDG_CACHE_HOME": f"{home}/xdg-cache",
        "XDG_STATE_HOME": f"{home}/xdg-state",
        "CLAUDE_CONFIG_DIR": f"{home}/claude-config",
        "ANTHROPIC_BASE_URL": f"http://127.0.0.1:{port}",
        "ANTHROPIC_API_KEY": PLACEHOLDER_KEY,
        # A closed loopback port: anything that ignores NO_PROXY fails here.
        "HTTPS_PROXY": "http://127.0.0.1:9",
        "HTTP_PROXY": "http://127.0.0.1:9",
        "NO_PROXY": "127.0.0.1,localhost",
        "DISABLE_TELEMETRY": "1",
        "DISABLE_AUTOUPDATER": "1",
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        "TERM": "dumb",
    }


def write_result(result: dict):
    with open(f"{WORK}/result.json", "w", encoding="utf-8") as f:
        json.dump(result, f, indent=2)


def list_modules() -> int:
    """Host side, for the runtime manifest: the standard library files this
    script loads when it runs its self-test. Makes no outbound connection."""
    self_test(start_server())
    files = sorted({m.__file__ for m in list(sys.modules.values()) if getattr(m, "__file__", None) and m.__file__.startswith(STDLIB + "/")})
    json.dump(files, sys.stdout)
    return 0


def main() -> int:
    if sys.argv[1:] == ["--list-modules"]:
        return list_modules()
    if sys.argv[1:] == ["--child"]:
        print("child ran")
        return 0
    with open(f"{INPUT}/plan.json", encoding="utf-8") as f:
        plan = json.load(f)
    server = start_server()
    result = {"probe": probe(plan, server), "runs": []}
    write_result(result)
    if not result["probe"]["ok"]:
        print("sandbox probe failed, the executable was not run", file=sys.stderr)
        return 2
    env = executable_environment(server.server_address[1])
    result["environment_given_to_executable"] = {**env, "ANTHROPIC_BASE_URL": "http://127.0.0.1:<port>"}
    for run in plan["runs"]:
        os.makedirs(env["HOME"], exist_ok=True)
        os.makedirs(f"{WORK}/cwd", exist_ok=True)
        with Recorder.lock:
            Recorder.records.clear()
        try:
            done = subprocess.run(
                [f"{INPUT}/claude", *run["args"]],
                cwd=f"{WORK}/cwd",
                env=env,
                stdin=subprocess.DEVNULL,
                capture_output=True,
                timeout=plan["timeout_seconds"],
                check=False,  # a non-zero exit is part of what is recorded
            )
            outcome = {"exit_code": done.returncode, "stdout": done.stdout.decode("utf-8", "replace"), "stderr": done.stderr.decode("utf-8", "replace")}
        except subprocess.TimeoutExpired as e:
            outcome = {
                "timed_out_after_seconds": plan["timeout_seconds"],
                "stdout": (e.stdout or b"").decode("utf-8", "replace"),
                "stderr": (e.stderr or b"").decode("utf-8", "replace"),
            }
        with Recorder.lock:
            requests = list(Recorder.records)
        result["runs"].append({"label": run["label"], "args": run["args"], **outcome, "requests": requests})
        write_result(result)
    write_result(result)
    return 0


if __name__ == "__main__":
    sys.exit(main())
