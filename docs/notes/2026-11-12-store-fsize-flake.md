# The store/dir test that failed CI after passing

Date: 2026-11-12

CI run 43194668481 on `90d424f` failed in its tests step. Every test of `src/store/dir` passed, and the package was then reported as failed:

```
PASS
testing: can't write /tmp/go-build3693687670/b689/testlog.txt: write /tmp/go-build3693687670/b689/testlog.txt: file too large
FAIL	github.com/gfx-labs/acpharness/src/store/dir	8.336s
```

The second attempt of the same run passed. `90d424f` does not touch `src/store`.

## Cause

`TestFailedAppendLeavesNoTornRecord` made an append fail part way by lowering the file size limit (`RLIMIT_FSIZE`) of the test process to a few bytes past the session log, and raised it again after the append. The limit is the whole process's. `go test` has the test binary write a log file of its own (`-test.testlogfile`), which was 4.4 MB in these runs, far over the lowered limit. A write to it while the limit was low was refused, and the binary reported that when it finished.

Measured with the test binary run directly, 60 runs each:

| build | flags | failed |
| --- | --- | --- |
| `3c065d4`, the commit before | `-test.testlogfile` | 8 |
| `90d424f` | `-test.testlogfile` | 7 |
| `90d424f` | no test log | 0 |
| `90d424f` | `-test.testlogfile`, without this test | 0 |

So it needs both the test log and this test, and it was there before `90d424f`.

## Fix

The append under the lowered limit is made by a second process: the test binary run again with `ACPHARNESS_TEST_LIMIT_*` set, the way `TestMovedSessionContinues` already runs one. That process writes nothing but the session log. It succeeds only when the append failed with `EFBIG`. The test process then reads the log, as before, and checks that it is unchanged and that the next append works.

- 120 runs with `-test.testlogfile`: 0 failed.
- With the rollback in `Store.append` taken out (`go test -overlay`), both cases of the test fail, so it still catches a torn record.
- `go test -race -cover ./src/store/dir` passes.

No production code changed.
