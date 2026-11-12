# MCP stdio: a process outside the group must not hold Close

2026-11-12

## What was wrong

`Manager.Close`, the idle close and the reconnect after a server died all wait for the stdio server to be reaped. Reaping waited for the end of the server's stdout and stderr. The end of a pipe comes when the last process holding it exits, and the group kill reaches only the server's process group. A server that starts something in a session or group of its own (`setsid`, a daemon, a browser) and leaves it the pipes kept all three paths waiting for as long as that process ran.

Measured with a server whose tool starts `sleep` under `Setsid` holding one of the pipes: `Close` had not returned after 10 seconds for either pipe. It returns in 0.44 seconds with the fix, the 200 ms test grace included.

## What changed

- `process.closePipes` closes our ends of stdout and stderr. A read that is blocked on them returns.
- `shutdown` calls it after the final SIGKILL, and `stop` calls it after its kill. Both come after the server has had its chance to exit, so nothing the server still wants to write is lost.
- When the server goes away on its own, the reaper gives the stderr reader `mcp.terminate_grace_seconds` to reach the end, then closes the pipes. The last lines a crashing server wrote are still logged, and the server is reaped within one grace period.

The process outside the group is not signalled. It left the group on purpose, and the shell tool leaves such a process alone as well (`PipeHeld`).

## Test

`TestCloseDoesNotWaitForAProcessOutsideTheGroup` in `src/mcp/manager_test.go`, for stdout and for stderr. It fails on the transport as it was.
