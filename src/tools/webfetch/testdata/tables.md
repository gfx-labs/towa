# Signal reference

Signals a daemon is expected to handle

| Signal | Number | Usual meaning |
| --- | --- | --- |
| `SIGHUP` | 1 | Reload, or [reopen]({{BASE}}/glossary#reopen) logs |
| `SIGUSR1` | 10 | Reopen logs \| dump state |
| `SIGTERM` | 15 | Stop.<br>Flush first. |
| Numbers are for Linux on x86 |  | See signal(7) |

## Without a header row

|  |  |  |
| --- | --- | --- |
| app.log | 4.1 MB |  |
| app.log.1 | 38 MB | compressed |

## Header cells in the first row

| Tool | Reopens |
| --- | --- |
| logrotate | with *postrotate* |

## A table used for layout

### Left column

Text in the left column.

| Key | Value |
| --- | --- |
| rotate | 7 |
