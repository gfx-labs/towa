# An API key from a command

2026-11-12

An `api_key` account names where its key comes from in one of two ways:
`api_key_env`, an environment variable, or `api_key_command`, a program and its
arguments that print the key. It sets exactly one of them. The command is for a
key held by a password manager or a secrets tool, so that the key is in no file
and in no environment that every child process inherits.

```yaml
accounts:
  - id: vault
    provider: openrouter
    kind: api_key
    api_key_command: [pass, show, openrouter/key]
```

## What it does

- The command is a list, as a hook's is, and is run as it is with no shell. A
  pipeline is written as `[sh, -c, "..."]`.
- It runs once, when the credential source is built: at the start of `serve`
  and of `usage`. The key is kept in memory for the life of the process and
  is written nowhere. `accounts`, `config` and `hosts` do not run it.
- It runs with towa's environment and no standard input. The key is its
  standard output without the space around it.
- A command that exits with an error, prints nothing, or takes longer than
  `auth.key_command_timeout_seconds` (default 60) stops the start. The default
  leaves time for a tool that waits for a hardware key touch. The error names
  the account and the program and carries what the command wrote to standard
  error. It carries neither the arguments nor the output, since either may
  hold a secret.
- `towa config` prints the program and redacts its arguments, as it does for
  a hook.

## What it does not do

- The command is not run again when the provider refuses the key. A key that
  is rotated while towa runs needs a restart. This is the same as a key from
  the environment (`auth.ErrStaticKey`).
- Every account with a command runs it at the start, whether or not a model
  chain names the account.

## Checks

`TestAnAPIKeyFromACommand` in `src/auth` runs real commands: one that prints a
key (and is counted, to show it runs once for several requests), one that
fails, one that prints nothing and one that outlives its timeout.

Run by hand on the built binary with a scratch config:

| case | result |
|---|---|
| `serve`, command prints a key | serves, command ran once |
| `serve`, command exits 2 after writing `vault is locked` | exit 1, `account "vault": api_key_command sh: exit status 2: vault is locked` |
| both `api_key_env` and `api_key_command` | config error |
| neither | config error |
| `accounts`, `config` | command not run |
