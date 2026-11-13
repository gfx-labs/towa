# The edit tool takes a list of edits

2026-11-12

## Why

`edit` replaced one string per call. A change at several places of one file took one request per place, or `apply_patch`, or a script in `bash`, which the edit rules forbid.

Seen on `oku-trade/trade` PR 4673 with `deepseek/deepseek-v4.1-flash-1017` through Morph on OpenRouter, towa master `a1b2323`, one run (`eval20-4673-master-1`):

| Phase | towa master | towa `pi-prompt` branch | pi 1.3.0 |
|---|---|---|---|
| Reading | 5 | 8 | 5 |
| Making the change | 12 | 1 | 1 |
| Checking | 11 | 5 | 5 |
| Final reply | 1 | 1 | 1 |
| Requests | 29 | 15 | 12 |

- Master read the file with `cat`, so its first two `edit` calls were refused with "File has not been read yet". It then made seven single replacements over eleven requests, with re-reads between them.
- pi and the branch each wrote the whole change as one Python script in `bash`. Neither has the rule against script edits. pi's own `edit` takes a list, and pi did not use it here.

## What changed

- `edit` takes `file_path` and `edits`, a list of `old_string`, `new_string` and `replace_all`. At least one edit is required.
- The edits are applied in order, each to the result of the ones before it. Nothing is written unless every edit applies. The file layer already did this (`fsops.ApplyEdits`); the tool passed it one edit.
- There is no top-level `old_string` and `new_string`. jcode keeps both forms, and a model given two ways to write one thing can go on using the one-per-call form. A call in the old shape is refused by the schema, which names the two members it does not take.
- One object sent as `edits` where the list is declared is repaired to a list of one, by the argument repair that every tool has.
- A failure names the edit: `edits[1].old_string was not found in the file`, with a hint that an earlier edit of the call may have changed the text when the index is not 0. It says the file was not changed.
- An empty `old_string` no longer creates a file. `edit` changes a file that exists and says to use `write` otherwise. One way to create a file is enough.
- The rules gain pi's line, with towa's names: "When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls." `apply_patch`'s rule now speaks of several files only.

The shape follows the three harnesses that have a list:

| | Members of one edit | Order | On a failure |
|---|---|---|---|
| jcode `037163c7b` | `old_string`, `new_string`, `replace_all` | in order, later edits see earlier ones | nothing written |
| pi 1.3.0 | `oldText`, `newText` | every edit matched against the original file | nothing written |
| towa | `old_string`, `new_string`, `replace_all` | in order, later edits see earlier ones | nothing written |

Codex's own file tool is `apply_patch`, which towa also has.

## The pi-prompt branch

Before this, a branch (`pi-prompt`, `d6b2073`, not merged) gave towa pi's system prompt: its opening, its rules reworded for towa's tools, the rule files in an `<addendum>` without paths, its XML skills list. The tools and their definitions were unchanged. The comparison was stopped after the runs below, all on the DeepSeek model above at medium effort (`eval20-*`).

| PR | towa master | towa `pi-prompt` | pi |
|---|---|---|---|
| 4668 | 13 | 12, 15 | 9, 12 |
| 4672 | 9 | 9 (fails 2 of 2), 8 | 9, 11 (fails 2 of 2) |
| 4673 | 29 | 15 (fails 1 of 3), 14 | 12, 14 |

Requests per run. A run passes the merged PR's tests unless marked.

- On 4668 and 4672 the prompt made no visible difference to the request count.
- On 4673 the branch took half of master's requests because, without the rule against script edits, it edited with a script as pi did. That is the rule's cost with a one-replacement `edit`, not a gain from pi's wording.
- The branch is not adopted.

## Not measured

- Whether a model uses the list. No task has been run on this build.
- The read-before-edit refusal is unchanged. A model that reads with `cat` still loses a request to it.
