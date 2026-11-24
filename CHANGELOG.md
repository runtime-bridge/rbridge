# Changelog

## v1.0.0 (unreleased)

- More providers over ACP: Codex (codex-acp), Gemini CLI and the Claude ACP adapter. Providers whose terms rule out using an account login from another tool need an API key; detection reports `needs-api-key` until one is set.
- Any ACP agent can be added through `providers.custom` alone; it passes the same contract suite as the built-in adapters.
- `rbridge mcp`: rbridge as an MCP server for other agents.
- `rbridge config` prints the effective configuration and its layers; `providers.<id>.model` sets a default model and `providers.<id>.args` replaces launch arguments.
- SDKs: `@rbridge/sdk` for Node.js and `rbridge` for Python, both without dependencies.
- Install script that verifies checksums and, when cosign is present, signatures.
- Docs: getting started, configuration reference, threat model, performance, releasing.
- The daemon moves its socket to a short temp path when the runtime directory is too deep for a Unix socket; clients find it through `daemon.json`.

## v0.2.0 (unreleased)

- `rbridge daemon start|stop|status|run`: one daemon for several applications.
- IPC over a Unix domain socket (JSON-RPC, `specs/ipc-protocol.md`) and optional local HTTP with Server-Sent Events (`specs/http-api.yaml`).
- Per-client session ownership, event replay, approvals answered by the client that owns the session.
- `bridge.Connect` and the `bridge.Client` interface shared with the embedded `Bridge`.
- Sessions idle for `daemon.session_idle_timeout` are closed; sessions left by a crash show as interrupted.

## v0.1.0 (unreleased)

First release: the CLI and the Go library.

- Detects Claude Code, OpenCode and custom ACP agents; `providers`, `doctor`.
- Runs tasks and multi-turn sessions on Claude Code (headless stream-json) and on any ACP agent; `run`, `chat`, `sessions`.
- Skill library with digests, trust states, Git sources pinned to a commit, and per-session delivery; `skills`.
- Policy profiles `observe`, `standard`, `controlled` and `custom`, with an enforcement report per session and approvals that time out to deny.
- Audit log with one record per session; `audit`.
- Go library `github.com/rbridge/rbridge/pkg/bridge`.
