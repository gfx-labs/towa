"""Find traces of this host, of the capture sandbox and of one session in text
recorded from the Claude Code executable.

Each match has a class. What a class means for a given text is the caller's
decision: a sandbox path is expected in a raw request and is not acceptable in
a tool definition that gets published.
"""

import base64
import os
import pwd
import re
import socket

# A host or user name shorter than this is not searched for as a bare word: a
# one-letter name such as "a" matches ordinary text and says nothing. It is
# still found where it identifies the host, inside a /home/<name> path.
MIN_BARE_NAME = 3

CLASSES = {
    "credential": "key-shaped text other than the placeholder key",
    "host_home_path": "a /home/<name> path at the root of the filesystem (the sandbox has no /home)",
    "host_user_name": "this host's user name as a whole word",
    "host_name": "this host's name as a whole word",
    "host_network_interface": "the name of one of this host's network interfaces",
    "host_kernel": "this host's kernel release, or any Linux kernel version",
    "generated_identifier": "a UUID or a 64-digit hex value, as generated for a device or session",
    "sandbox_path": "a path under the sandbox's /work or /input",
    "calendar_date": "a month name followed by a year, as the executable writes the date of the run into some texts",
}
# Classes that mean the sandbox let something of the host's identity through.
HOST_IDENTITY = ("credential", "host_home_path", "host_user_name", "host_name", "host_network_interface")

_WORD = r"A-Za-z0-9_"
_PATH = r"A-Za-z0-9_.\-"
_MONTHS = "January|February|March|April|May|June|July|August|September|October|November|December"


def host_facts() -> dict:
    uname = os.uname()
    return {
        "user": pwd.getpwuid(os.getuid()).pw_name,
        "host": socket.gethostname(),
        "kernel_release": uname.release,
        "interfaces": sorted(name for _, name in socket.if_nameindex() if name != "lo"),
    }


def skipped_checks(facts: dict) -> list:
    """Bare-word checks that are not made because the name is too short."""
    out = []
    for label, value in (("user name", facts["user"]), ("host name", facts["host"])):
        if len(value) < MIN_BARE_NAME:
            out.append(f"{label} as a bare word: it has {len(value)} character(s), under {MIN_BARE_NAME}")
    out += [f"interface name {name!r} as a bare word: under {MIN_BARE_NAME} characters" for name in facts["interfaces"] if len(name) < MIN_BARE_NAME]
    return out


def _word(value: str) -> str:
    return rf"(?<![{_WORD}]){re.escape(value)}(?![{_WORD}])"


def find(text: str, facts: dict, placeholder_key: str) -> list:
    """[{"class", "match", "offset"}] for every trace found in text."""
    patterns = [
        ("credential", r"sk-ant-[A-Za-z0-9_-]+"),
        # Not preceded by a path character, so /work/home/... does not match.
        ("host_home_path", rf"(?<![{_PATH}/])/home/[{_PATH}]+"),
        ("host_kernel", rf"{re.escape(facts['kernel_release'])}|Linux \d+\.\d+\.\d+[{_PATH}+]*"),
        ("generated_identifier", r"(?<![0-9A-Fa-f-])[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(?![0-9A-Fa-f-])"),
        ("generated_identifier", r"(?<![0-9A-Fa-f])[0-9a-f]{64}(?![0-9A-Fa-f])"),
        ("sandbox_path", rf"(?<![{_PATH}/])/(?:work|input)(?:/[{_PATH}/]*)?(?![{_PATH}])"),
        ("calendar_date", rf"(?<![{_WORD}])(?:{_MONTHS}) (?:\d{{1,2}}, )?\d{{4}}(?![{_WORD}])"),
    ]
    if len(facts["user"]) >= MIN_BARE_NAME:
        patterns.append(("host_user_name", _word(facts["user"])))
    if len(facts["host"]) >= MIN_BARE_NAME:
        patterns.append(("host_name", _word(facts["host"])))
    patterns += [("host_network_interface", _word(name)) for name in facts["interfaces"] if len(name) >= MIN_BARE_NAME]
    found = []
    for name, pattern in patterns:
        for match in re.finditer(pattern, text):
            if name == "credential" and match.group() == placeholder_key:
                continue
            found.append({"class": name, "match": match.group(), "offset": match.start()})
    # The exact release and the general kernel pattern can hit the same place.
    seen = set()
    unique = []
    for item in sorted(found, key=lambda f: (f["offset"], -len(f["match"]))):
        if any(item["class"] == c and start <= item["offset"] < end for c, start, end in seen):
            continue
        seen.add((item["class"], item["offset"], item["offset"] + len(item["match"])))
        unique.append(item)
    return unique


def counts(found: list) -> dict:
    """Matches per class, without the matched text."""
    out = {}
    for item in found:
        out[item["class"]] = out.get(item["class"], 0) + 1
    return dict(sorted(out.items()))


# Headers that may only ever carry the placeholder key.
CREDENTIAL_HEADERS = ("authorization", "x-api-key", "cookie", "proxy-authorization")
# Expected in a raw request and not a sign of a leak: the sandbox's own paths,
# identifiers the executable generates in its empty config directory, the
# kernel release, which the sandbox cannot hide, and the date of the run.
# scan_runs counts them and does not block on them. Anything extracted for the
# repository may hold none.
NOTED_CLASSES = ("sandbox_path", "generated_identifier", "host_kernel", "calendar_date")


def scan_runs(runs: list, placeholder_key: str) -> dict:
    """What the executable sent and printed in the recorded runs, checked for
    credentials and for traces of this host. A "blocking" entry means the
    recording must not be used."""
    facts = host_facts()
    blocking = []
    noted = {}

    def check_text(where: str, text: str):
        for name, count in counts(find(text, facts, placeholder_key)).items():
            if name in NOTED_CLASSES:
                noted.setdefault(where, {})[name] = count
            else:
                blocking.append(f"{where}: {count} match(es) of class {name} ({CLASSES[name]})")

    for run in runs:
        for stream in ("stdout", "stderr"):
            check_text(f"run {run['label']} {stream}", str(run.get(stream, "")))
        for index, request in enumerate(run["requests"]):
            where = f"run {run['label']} request {index} ({request['method']} {request['path']})"
            for name, value in request["headers"]:
                if name.lower() in CREDENTIAL_HEADERS and value not in (placeholder_key, f"Bearer {placeholder_key}"):
                    blocking.append(f"{where}: header {name} is not the placeholder")
            if "body_base64" in request:
                blocking.append(f"{where}: the body is not UTF-8 and was not scanned")
                body = base64.b64decode(request["body_base64"]).decode("utf-8", "replace")
            else:
                body = request["body"]
            check_text(where, request["path"] + "\n" + "\n".join(f"{k}: {v}" for k, v in request["headers"]) + "\n" + body)
    return {"blocking": blocking, "noted_counts_by_class": noted, "checks_not_made": skipped_checks(facts), "classes": CLASSES}
