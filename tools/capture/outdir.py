"""Install a finished capture into its destination without emptying it.

Other tools read the capture directories while a capture script runs. A script
builds its output in a scratch directory and calls install(), which changes
only the files that differ.
"""

import os
import tempfile
from pathlib import Path


class InstallError(Exception):
    pass


def tree(root: Path) -> dict:
    return {p.relative_to(root).as_posix(): p.read_bytes() for p in sorted(root.rglob("*")) if p.is_file()}


def install(stage: Path, out: Path, owned=None):
    """Make out hold the files of stage.

    owned: the top-level entries of out that belong to the caller. Files under
    other entries are left alone. None means the whole of out belongs to it.

    A file that already has the staged bytes is not touched. A changed or new
    file is put in place in one rename. A file the new capture does not have is
    removed after everything else is in place."""
    staged = tree(stage)
    if owned is not None:
        stray = [rel for rel in staged if rel.split("/")[0] not in owned]
        if stray:
            raise InstallError(f"the capture wrote {stray[0]}, which is not under one of {list(owned)}")
    current = {}
    if out.is_dir():
        for entry in sorted(out.iterdir()):
            if owned is not None and entry.name not in owned:
                continue
            if entry.is_dir():
                current.update({f"{entry.name}/{k}": v for k, v in tree(entry).items()})
            elif entry.is_file():
                current[entry.name] = entry.read_bytes()
    for rel, data in staged.items():
        if current.get(rel) == data:
            continue
        target = out / rel
        target.parent.mkdir(parents=True, exist_ok=True)
        fd, temporary = tempfile.mkstemp(dir=target.parent, prefix=f".{target.name}.")
        with os.fdopen(fd, "wb") as f:
            f.write(data)
        os.replace(temporary, target)
    for rel in current:
        if rel not in staged:
            (out / rel).unlink()
            parent = (out / rel).parent
            while parent != out and not any(parent.iterdir()):
                parent.rmdir()
                parent = parent.parent
