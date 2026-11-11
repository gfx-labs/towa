"""Runs inside the bubblewrap sandbox started by claude_code_record.py.

Standard library only: the sandbox root holds the system Python, the shared
libraries it and the executable load, and the run directory at /work. It checks the sandbox, starts a recording server on loopback,
runs the Claude Code executable against it and writes what the server received.

The server answers every request with HTTP 400, so no model output exists
anywhere in the result. Only the request the CLI builds is recorded.
"""

import http.server
import json
import os
import socket
import subprocess
import sys
import threading
from pathlib import Path

PLACEHOLDER_KEY = "sk-ant-placeholder-not-a-real-key"
# Everything the sandbox root may contain. Anything else fails the probe.
ROOT_ENTRIES = ["dev", "lib", "lib64", "proc", "tmp", "usr", "work"]
ABSENT = ["/home", "/etc", "/run", "/root", "/mnt", "/nix", "/var", "/sys", "/srv", "/opt", "/media", "/boot"]


def probe(run_dir: Path) -> dict:
    """What the sandbox can see and reach. ok is False when any check fails."""
    links = sorted(name for _, name in socket.if_nameindex())
    connects = {}
    for host, port in (("1.1.1.1", 443), ("127.0.0.53", 53)):
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
    present = {p: os.path.lexists(p) for p in ABSENT}
    files = []
    for top in ("/usr",):  # /lib and /lib64 are symlinks into it
        for base, _, names in os.walk(top):
            # The Python standard library is one bound directory; list it as one entry.
            if base.startswith("/usr/lib/python3"):
                continue
            files.extend(os.path.join(base, n) for n in names)
    stdlib = sorted(d for d in os.listdir("/usr/lib") if d.startswith("python3"))
    ok = (
        links == ["lo"]
        and all(v.startswith("failed") for v in connects.values())
        and dns.startswith("failed")
        and root == ROOT_ENTRIES
        and not any(present.values())
        and os.listdir("/tmp") == []
        and str(run_dir) == "/work"
    )
    return {
        "ok": ok,
        "network_links": links,
        "outbound_connects": connects,
        "dns_lookup_api_anthropic_com": dns,
        "root_listing": root,
        "paths_that_must_be_absent": present,
        "files_under_usr": sorted(files),
        "root_symlinks": {p: os.readlink(p) for p in ("/lib", "/lib64")},
        "python_standard_library_directories": stdlib,
        "work_listing": sorted(os.listdir(run_dir)),
        "hostname": socket.gethostname(),
        "uid": os.getuid(),
        "environment_names": sorted(os.environ),
    }


class Recorder(http.server.BaseHTTPRequestHandler):
    records = []
    lock = threading.Lock()

    def handle_any(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        with self.lock:
            self.records.append(
                {"method": self.command, "path": self.path, "headers": [[k, v] for k, v in self.headers.items()], "body": body.decode("utf-8")}
            )
        answer = json.dumps({"type": "error", "error": {"type": "invalid_request_error", "message": "recording server: no model here"}}).encode()
        self.send_response(400)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(answer)))
        self.end_headers()
        self.wfile.write(answer)

    do_GET = do_POST = do_PUT = do_HEAD = do_DELETE = do_PATCH = do_OPTIONS = handle_any

    def log_message(self, *args):
        pass


def main() -> int:
    run_dir = Path(sys.argv[1])
    plan = json.loads((run_dir / "plan.json").read_text())
    result = {"probe": probe(run_dir), "runs": []}
    out = run_dir / "result.json"
    if not result["probe"]["ok"]:
        out.write_text(json.dumps(result, indent=2))
        print("sandbox probe failed, the executable was not run", file=sys.stderr)
        return 2
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Recorder)
    port = server.server_address[1]
    threading.Thread(target=server.serve_forever, daemon=True).start()
    home = run_dir / "home"
    cwd = run_dir / "cwd"
    env = {
        "PATH": "/usr/bin",
        "HOME": str(home),
        "XDG_CONFIG_HOME": str(home / "xdg-config"),
        "XDG_DATA_HOME": str(home / "xdg-data"),
        "XDG_CACHE_HOME": str(home / "xdg-cache"),
        "XDG_STATE_HOME": str(home / "xdg-state"),
        "CLAUDE_CONFIG_DIR": str(home / "claude-config"),
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
    for run in plan["runs"]:
        home.mkdir(exist_ok=True)
        cwd.mkdir(exist_ok=True)
        with Recorder.lock:
            Recorder.records.clear()
        try:
            done = subprocess.run(
                [str(run_dir / "claude"), *run["args"]], cwd=cwd, env=env, stdin=subprocess.DEVNULL, capture_output=True, timeout=plan["timeout_seconds"]
            )
            outcome = {"exit_code": done.returncode, "stdout": done.stdout.decode("utf-8", "replace"), "stderr": done.stderr.decode("utf-8", "replace")}
        except subprocess.TimeoutExpired as e:
            outcome = {"timed_out_after_seconds": plan["timeout_seconds"], "stdout": (e.stdout or b"").decode("utf-8", "replace"), "stderr": (e.stderr or b"").decode("utf-8", "replace")}
        with Recorder.lock:
            requests = list(Recorder.records)
        result["runs"].append({"label": run["label"], "args": run["args"], **outcome, "requests": requests})
        out.write_text(json.dumps(result, indent=2))
    result["environment_given_to_executable"] = {k: (v if k != "ANTHROPIC_BASE_URL" else "http://127.0.0.1:<port>") for k, v in env.items()}
    out.write_text(json.dumps(result, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
