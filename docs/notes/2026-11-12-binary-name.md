# The binary is `acph`

Date: 2026-11-12

The command is now `acph`, built from `cmd/acph`. `acpharness` was long to type for a command run by hand and from scripts.

Changed: the `cmd` directory, the name kong prints in usage and help, the `acph:` prefix on a fatal error, and every message that tells the user a command to run (`acph login ...`, `acph mcp login ...`).

Not changed, because they name the project and not the command:

- the repository and the Go module path
- the config and state directories (`/etc/acpharness`, `~/.config/acpharness`, `~/.local/state/acpharness`)
- the `ACPHARNESS_` environment prefix
- the `acpharness` key under `_meta` and in error data
- what is sent to other parties: the user agent, the Codex `originator`, the MCP client name, the ACP agent name, the OTel service name and metric names

Earlier notes still say `acpharness <command>`. They are dated records and were left as written.
