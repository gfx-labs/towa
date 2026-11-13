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
- A second commit adds pi's other two edit rules. pi's edits are matched against the original file and towa's against the result of the earlier edits, so the first is reworded: "Each edits[].old_string is matched against the file as the earlier edits of the call left it. Do not emit overlapping or nested edits. Merge nearby changes into one edit." The second, "Do not pad with large unchanged regions.", is added to the line about keeping `old_string` small. The four lines about the list now come before the lines about reading the file.

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

## After the change

towa `c42f04c` alone on PR 4673, same model, two runs (`eval21-4673-multiedit-1` and `-2`). The pi figures are the two runs above, not run alongside.

| Run | Requests | Tokens | Seconds | `edit` calls | Replacements per call | PR tests |
|---|---|---|---|---|---|---|
| towa `a1b2323`, before | 29 | 1,196,222 | 309 | 9 | 1 | 3 of 3 |
| towa `c42f04c`, run 1 | 23 | 894,201 | 302 | 2 | 7, 2 | 3 of 3 |
| towa `c42f04c`, run 2 | 14 | 550,038 | 219 | 2 | 6, 5 | 3 of 3 |
| pi, run 1 | 12 | 473,013 | 84 | 0 | script in `bash` | 3 of 3 |
| pi, run 2 | 14 | 624,523 | 86 | 0 | script in `bash` | 3 of 3 |

- The model used the list in both runs without being corrected.
- Run 2's first `edit` was refused with "File has not been read yet": it had read the file with `cat`. It read the file and sent the edits again.
- Run 1's second `edit` moved an import line its first call had put in the wrong place, and added one line.
- Run 1 made 13 of its 25 tool calls after the edits, four of them `lint:browser-agent`. Its count is in checking, not in editing.
- Three `old_string` values were whole call expressions of 12 or 13 lines (349 and 465 characters), where the change was to a line or two of them.

## Not measured

- The effect of the two rules added in the second commit. The runs above are on `c42f04c`, which does not have them.
- The read-before-edit refusal is unchanged. A model that reads with `cat` still loses a request to it.
- Each cell is one or two runs, and run 1 and run 2 differ by 9 requests on the same build.
