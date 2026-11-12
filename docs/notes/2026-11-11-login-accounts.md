# Login creates the account, and logins live in auth.json

2026-11-11

## Why

`towa login` refused any account that a config file did not already declare, and no command made one. A first login was therefore impossible without editing a config file by hand. The user asked for account creation from the command line, a printed page to click or a device code with its page, logins kept in a file of their own (`auth.json`), and jcode's two-run login, where a second run of the command takes the code.

## What changed

- **One file.** `$XDG_STATE_HOME/towa/auth/auth.json` holds every OAuth account by id: its provider and its tokens. It replaces one `<account>.json` per account. `src/auth/store.go`.
- **A login creates the account.** `towa login <provider> --account <id>` needs no `accounts` entry. `accounts` in the configuration is now for API keys and proxy-held accounts.
- **A chain may name an account first.** An account that a model chain names on `claude` or `codex` is taken as an oauth account, so a config can be written before the login. A turn that reaches it reports `ErrNotLoggedIn` with the command to run. `config.Sources.StoredAccounts` carries the store's accounts into `config.Load`, and `Account.Stored` marks an account that no config layer declares.
- **The page is always printed.** A browser login used to fail when no opener could be started. It now prints the page, says no browser opened, and keeps waiting for the callback.
- **`--device`.** Codex only. Prints a one-time code and `https://auth.openai.com/codex/device`, then asks for the approval until it comes. `src/auth/device.go`.
- **`--code`.** A browser or manual login writes `auth/pending/<provider>.json` before it shows its page. `towa login <provider> --code '<code or address>'` in a later run finishes it. The record names the account, so the later run needs the provider alone. `src/auth/pending.go`.
- **A manual login with nobody at the terminal.** An empty line or a closed input leaves the pending record, prints the command for the later run and exits 0. This is how a script or an agent starts a login that a person finishes.
- `towa hosts` names a stored account as `auth.json:accounts.<id>` and adds a flow's device endpoints. Codex's are on `auth.openai.com`, which the authorize and token endpoints already put in the list, so the list for Codex is the same as before.
- A browser login that times out names the `--code` run in its error. Its pending record is still there.
- The examples in `docs/design.md` are YAML. The sketch there did not load as written: `limits.subagent_depth` and `limits.subagent_concurrency` are not keys (the section is `subagents`, with `max_depth` and `max_concurrent`), the hook had no `timeout`, and no provider `openrouter` was declared for the chain entry and account that name it. All three are corrected.

## Decisions

- **Two locks.** `account.<id>.lock` is held across a refresh, as before, so one refresh serves every process and one account's token request does not hold up another's. `auth.json.lock` is new and is held only for the read and write of the file, because the file now holds every account and two writers would otherwise each drop the other's change. A read takes no lock: the file is replaced by rename.
- **`auth.json.lock` takes no context.** A refreshed token must be stored even if its turn was cancelled, which was already the rule for the write.
- **One pending login per provider.** A new login replaces the record. jcode can also scope a record with `--flow-id`. Not built: nothing here starts two logins of one provider at once.
- **The pending record has no expiry.** jcode gives its record one. Here a stale record fails at the provider when its code is exchanged. Revisit if a stale record is found to cause a confusing error.
- **A declared account wins.** An id in both the configuration and `auth.json` is the configured account. A stored login for another provider is reported when the account is used, as before.
- **`logout` of a stored account removes the account.** It exists only as its entry.
- **No fallback between the ways to log in.** `--manual`, `--device` and `--code` are refused together, and a device login on a provider without one is an error.
- **A missing polling interval is an error.** Upstream defaults it to zero and would ask without a pause.
- **No dependency added.**

## Sources

- The device requests are `codex-rs/login/src/device_code_auth.rs` at openai/codex `ce0323f2f2d3b25e82806cd555237da3db4e1897`, the commit the Codex prompts are pinned to: `POST {issuer}/api/accounts/deviceauth/usercode` with the client id, `POST {issuer}/api/accounts/deviceauth/token` with the device id and user code, where 403 and 404 mean not approved yet, then the ordinary form code exchange with redirect `{issuer}/deviceauth/callback` and the verifier the approval carries. The 15 minutes are the time upstream waits and tells the user.
- Claude Code has no device grant. Its headless login is the page that shows `code#state`, which `--manual` and `--code` use.
- The two-run login follows `src/cli/login/scriptable.rs` in jcode (`--print-auth-url`, then `--callback-url` or `--auth-code`), with one flag for both kinds of pasted text, since `auth.ParsePasted` already reads either.

## The config format

The user asked whether the config should be YAML and not TOML. The loader reads `config.toml`, `config.yaml`, `config.yml` and `config.json` alike, and the session held no decision for one of them, so the documented examples moved to YAML and nothing was removed. Test fixtures in other packages are still TOML. Dropping the TOML parser is a separate decision.

## Checks

Every step of `.github/workflows/ci.yml`, run on the working tree and again on a clean checkout of the commit:

```
go build ./... && CGO_ENABLED=0 go build ./...
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./... && GOOS=darwin GOARCH=arm64 go vet ./...
go vet ./...
go generate ./...                                  # changes nothing
nice -n 10 go test -race -count=1 ./...
(cd tools/acpgen && go vet ./... && go test ./...)
go -C tools/conformance vet ./... && go -C tools/conformance test -race -count=1 ./...
nice -n 10 golangci-lint run ./...                 # 0 issues
```

- `TestLoginAccountsLogout` (`src/cli`): a config whose chain names an account with no login loads, `accounts` lists it as not logged in, a manual login with an empty input exits waiting, the later `--code` run creates the account, a `--code` run for another account is refused before any request, a used code has no pending login, and logout removes the created account.
- `TestLoginWithADeviceCode` (`src/auth`): the code and page are shown, a 403 is asked again after the interval, and the exchange uses the verifier and redirect of the device flow.
- `TestProcessesShareOneRefresh`: still one request for 12 processes and 12 goroutines, and the directory holds `account.work.lock`, `auth.json` and `auth.json.lock`, each 0600.
- `TestAccountsComeFromConfigStoreAndChains` (`src/config`): a declared account keeps its kind when the store has the same id, a stored account and a chain-named account on a built-in provider are added as oauth, and a chain that names an undeclared account on a configured provider is still an error.
- By hand, the built binary under `env -i HOME=<scratch>` with a YAML config, no opener on `PATH`:
  - `accounts` lists the chain-named account as not logged in.
  - `login codex --account gpt --manual` and `login claude --account work --manual` with the input closed each print their page and the `--code` run, exit 0 and leave `auth/pending/<provider>.json`, mode 0600, in directories of mode 0700.
  - `login claude --account work` prints the page, says no browser could be opened and waits. With `--set auth.login_timeout_seconds=1` it exits 1 and names the `--code` run.
  - `--device` on claude, `--manual` with `--device`, and `--code` with an `--account` other than the pending one are each refused, exit 1.
  - The three YAML blocks of `docs/design.md`, cut out of the file and passed with `--config`, load, and `accounts` lists `main`, `pod`, `chatgpt`, `work` and `personal`.
  - No request was sent in any of these.

## A test that failed once

The first whole run failed in `src/mcp`, at the last step of `TestCallDroppedOverHTTPIsUnknown`, which nothing in this change touches: a call to a server that was just stopped came back as unknown, where the test wants failed. The step stops the server and calls at once. When the client still holds a connection the server has closed, the request is written to it and ends in `EOF`, and unknown is the right report for that.

Measured with the test binary of the published commit `af4729c` and of the tree with the step changed to close the client's idle connections first, 8 processes of 150 runs at a time, the two builds in turn:

| build | failing runs of 1200 |
|---|---|
| `af4729c` | 205, 206, 176, 169 |
| idle connections closed first | 12, 9 |

So the change removes most of it and not all. All 21 that remain fail at the same last step. 12 report `http: server closed idle connection` and 1 reports `EOF`, so a connection outlived the close in those runs. Which one was not traced. The other 8 are in the gateway case alone and report `client is closing ... Unauthorized`. That reads as the SDK's session having closed after the concurrent call's 401 and refusing the next call, which `Manager.CallTool` then reports as unknown. Whether a request went out for that call was not traced either, so whether unknown is wrong there is an open question about `src/mcp`, not settled here. The published build shows the same message, 19 times in its 4800 runs. One run at a time, the changed test passed 30 of 30.

```
go test -race -c -o mcp.test ./src/mcp/
seq 8 | xargs -P 8 -I{} sh -c './mcp.test -test.run TestCallDroppedOverHTTPIsUnknown -test.count=150'
```

## Not done, not checked

- No live login. No request went to `claude.com`, `platform.claude.com` or `auth.openai.com`. The device requests follow upstream's source and were run only against a server the test runs.
- A turn that reaches an account with no login was not run. `auth.AccountSource` builds that error, with the command in it, and the CLI test above covers only `accounts`.
- Logins made before this change, in `<account>.json` files, are not read. None existed on the machine this was built on: no live login had been made.
- No ACP `authenticate` method. Login is a command, and an editor cannot start one.
- Nothing ran on macOS.
