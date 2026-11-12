# File lists are sorted here, not by ripgrep

Date: 2026-11-12

## What changed

`rg --sort path` makes ripgrep search on one thread. The search runner passed it for every mode. Now only lines mode passes it.

- Files mode, counts mode and `Find` run `rg` without `--sort`, read every file it reports, sort the list with `comparePaths`, then apply `Offset`, `Limit` and `tools.max_output_bytes`.
- Lines mode still passes `--sort path`. Its output can be very large (325 MB for the pattern `e` on the tree below), and ordered output is what lets the runner kill `rg` at the limit.

The order is unchanged. `comparePaths` gives the order `rg --sort path` gives: a directory's contents come where the directory's name sorts, so `sub/b.go` is before `sub b.go` and `sub-x/z.go`, which is not their order as plain text. `Offset` and `Limit` need a fixed order for paging, which is why the parallel order is not returned as it comes.

The modification time of a file is read only for the files that are kept, after sorting.

## Measurements

ripgrep 14.1.1, 8 cores, best of 15 calls of `search.Grep` or `search.Find` in process, old and new runner built with `go test -overlay` and run alternately twice. Tree A is a jcode checkout (2,840 files, 219 MB). Tree B is this repository (517 files).

| Call | Tree | Before | After |
|---|---|---|---|
| files, `runtimeFactory` (no file matches) | A | 44 ms | 12 to 13 ms |
| files, `fn` (2,000 files match), limit 250 | A | 10 to 11 ms | 17 to 18 ms |
| counts, `fn` (2,000 files), no limit | A | 62 to 63 ms | 22 ms |
| `Find`, all 2,840 files | A | 16 to 18 ms | 14 ms |
| lines, `fn`, limit 250 | A | 6 ms | 6 ms |
| lines, `runtimeFactory` (no match) | A | 46 ms | 46 ms |
| files, `runtimeFactory` (1 file) | B | 11 ms | 6 to 7 ms |
| files, `fn` (7 files) | B | 11 ms | 7 ms |
| counts, `e` (507 files) | B | 19 to 20 ms | 10 ms |

One case is slower. A files search whose pattern matches far more files than the limit used to stop `rg` after the first 250 in path order. It now waits for the whole search, 17 ms against 10 ms on tree A. A search that matches few files or none, which had to scan the whole tree on one thread, is the case that gains.

Lines mode with few or no matches still scans on one thread (46 ms on tree A). Running it in parallel would need all matches read before sorting, which is unbounded.

## Checks

A scratch test, not committed, compared `Find` and counts results with `rg --sort path` output on both trees for five patterns, and a files page at offset 3 with the same slice of the full list. All were equal. `TestFileListsAreInPathOrder` keeps the ordering rule and paging.
