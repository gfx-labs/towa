# Rotating log files *by hand*

Posted on 2 March 2026 by [Mira]({{BASE}}/authors/mira).

A daemon that keeps its log open will keep writing to the old file after you rename it. This note covers the **three** ways to deal with that, and when `copytruncate` is **not** the answer.

## Asking the daemon to reopen

Most daemons reopen their log on `SIGHUP` or `SIGUSR1`.
Check the manual first: for some, `SIGHUP` means *reload the configuration* and nothing else. See [the signal table]({{BASE}}/posts/reference.html#signals), the [logrotate manual](https://example.org/man/logrotate.8), or jump further down.

```sh
mv /var/log/app.log /var/log/app.log.1
kill -HUP "$(cat /run/app.pid)"
gzip /var/log/app.log.1   # only after the reopen
```

## Truncating in place

When the daemon cannot reopen, copy the file and cut the original to zero:

1. Copy the log.
2. Truncate the original with `: > app.log`.
   - Lines written between the two steps are ~~kept~~ lost.
   - The daemon must have opened the file with `O_APPEND`.
3. Compress the copy.

> If the file was not opened for appending, the daemon keeps its old offset and the file becomes sparse.
>
> — the `logrotate` manual, paraphrased

### What to check afterwards

- The new file is growing.
- Disk usage went down: `du -sh /var/log`.
- Open the live viewer

---

![File offsets before and after truncation]({{BASE}}/posts/img/offsets.png)

Figure 1. Offsets before and after.

Written for a test. Contact: [mira@example.org](mailto:mira@example.org)
