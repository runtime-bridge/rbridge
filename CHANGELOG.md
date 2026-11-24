# Changelog

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
