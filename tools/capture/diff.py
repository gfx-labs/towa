# /// script
# requires-python = ">=3.12"
# dependencies = []
# ///
"""Print what changed between two captures of the same upstream.

    uv run tools/capture/claude_code.py --version 2.1.304 --out "$JCODE_SCRATCH_DIR/capture/cc-2.1.304"
    uv run tools/capture/diff.py prompts/claude-code/2.1.280 "$JCODE_SCRATCH_DIR/capture/cc-2.1.304"

    uv run tools/capture/codex.py --ref <sha> --out "$JCODE_SCRATCH_DIR/capture/codex-new"
    uv run tools/capture/diff.py prompts/codex/ce0323f2 "$JCODE_SCRATCH_DIR/capture/codex-new"

Both arguments are directories written by a capture script. Files are matched
by path. A JSON file is compared as a value, because the Codex capture's member
order is not upstream's. Changes to each manifest's not_captured list are
printed as gaps. Exit status is 1 when anything differs.

A Claude Code file named <item>.vN is one of several outcomes of the same
item, numbered in the order found. A new release can renumber them, so such a
change shows as a change to several .vN files.
"""

import argparse
import difflib
import json
import sys
from pathlib import Path


def files(root: Path) -> dict:
    return {p.relative_to(root).as_posix(): p for p in sorted(root.rglob("*")) if p.is_file() and p.name != "manifest.json"}


def as_lines(path: Path) -> list:
    """Comparable lines of a file: JSON in a fixed layout, text as it is."""
    data = path.read_bytes()
    if path.suffix == ".json":
        return json.dumps(json.loads(data), indent=2, sort_keys=True, ensure_ascii=False).splitlines()
    try:
        return data.decode("utf-8").splitlines()
    except UnicodeDecodeError:
        return [f"<{len(data)} bytes, not UTF-8>"]


def same(a: Path, b: Path) -> bool:
    if a.suffix == ".json":
        return json.loads(a.read_bytes()) == json.loads(b.read_bytes())
    return a.read_bytes() == b.read_bytes()


def gaps(root: Path) -> dict:
    """What the capture's manifest says it could not capture, by name."""
    manifest = json.loads((root / "manifest.json").read_text())
    out = {}
    for entry in manifest.get("not_captured", []):
        if isinstance(entry, dict):
            out[f"{entry.get('kind', '')} {entry.get('name', '')}".strip()] = entry.get("reason", "")
        else:
            out[str(entry)] = ""
    return out


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("old", type=Path)
    parser.add_argument("new", type=Path)
    parser.add_argument("--names-only", action="store_true", help="list the files that differ without their text")
    opts = parser.parse_args()
    for root in (opts.old, opts.new):
        if not (root / "manifest.json").is_file():
            parser.error(f"{root} has no manifest.json: not a capture directory")

    old, new = files(opts.old), files(opts.new)
    removed = sorted(set(old) - set(new))
    added = sorted(set(new) - set(old))
    changed = [p for p in sorted(set(old) & set(new)) if not same(old[p], new[p])]

    for path in removed:
        print(f"removed  {path}")
    for path in added:
        print(f"added    {path}")
    for path in changed:
        print(f"changed  {path}")
    if not opts.names_only:
        for path in changed:
            print()
            sys.stdout.writelines(
                line + "\n" for line in difflib.unified_diff(as_lines(old[path]), as_lines(new[path]), f"old/{path}", f"new/{path}", lineterm="")
            )

    old_gaps, new_gaps = gaps(opts.old), gaps(opts.new)
    gap_changes = False
    # A gap that is gone is not proof the item is now captured: the capture
    # may have failed earlier, at a step the new manifest lists instead.
    for name in sorted(set(old_gaps) | set(new_gaps)):
        if name not in new_gaps:
            print(f"gap gone  {name}")
        elif name not in old_gaps:
            print(f"gap new   {name}: {new_gaps[name]}")
        else:
            continue
        gap_changes = True

    print(f"\n{len(removed)} removed, {len(added)} added, {len(changed)} changed, {len(set(old) & set(new)) - len(changed)} the same")
    return 1 if removed or added or changed or gap_changes else 0


if __name__ == "__main__":
    sys.exit(main())
