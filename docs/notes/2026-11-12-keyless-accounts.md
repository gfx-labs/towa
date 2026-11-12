# Accounts for an endpoint that takes no key

2026-11-12

## Why

A model server on the local network (LM Studio, Ollama, llama.cpp) takes no key. An account had to be `api_key` with a variable holding some made-up value, and the harness sent that value as a bearer token. The tests did the same with `FACTORY_TEST_KEY` and `OLLAMA_API_KEY`.

## What changed

- `accounts[].kind: none`. It has no other field.

  ```yaml
  providers:
    - id: lan
      kind: chat
      base_url: http://192.0.2.10:1234/v1
  accounts:
    - id: open
      provider: lan
      kind: none
  models:
    local:
      chain: ["lan:open/openai/gpt-oss-20b"]
      context_window: 131072
      max_output_tokens: 4096
  ```

- `auth.SchemeNone` is the credential of such an account. Its token is empty. The chat client, the responses client (HTTP and WebSocket, which share the header) and embeddings send no `Authorization` header for it. An empty `Bearer ` header is not sent, because some servers refuse a malformed one.
- The built-in `claude` and `codex` providers refuse the kind when the configuration loads. Both always take a credential, and the Anthropic client would have nothing to put in `X-Api-Key`.
- `Refresh` returns `auth.ErrNoCredential`, so a 401 from such an endpoint is an auth stop after one attempt, as for a static key.
- `acpharness accounts` shows the state `no credential`, and `login` says the account has no login.

## Checked

- `TestFactoryWriteProcess` now runs its account as `kind: none`. Its two recordings were made again through the built binary against an LM Studio server that takes no key, three recordings of three passing. The recorded requests hold no `Authorization` header, and the replay fails when a request has a header the recording does not, so the test fails if one is sent.
- `TestAccountsWithoutAStoredLogin` covers the credential and the refresh error.

## The model tests are recorded from

`openai/gpt-oss-20b`, served by LM Studio, is the model new recordings are made from (`AGENTS.md`). The recordings made earlier from `llama3.2:3b` on Ollama are unchanged: `src/loop/looptest`, the three provider packages and `src/compact`. They replay as they are, and each is recorded again from the new model when its request next changes.
