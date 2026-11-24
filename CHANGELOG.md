# Changelog

## v0.1.0 (unreleased)

First release: the CLI and the Go library.

- Detects Claude Code, OpenCode and custom ACP agents; `providers`, `doctor`.
- Runs tasks and multi-turn sessions on Claude Code (headless stream-json) and on any ACP agent; `run`, `chat`, `sessions`.
- Skill library with digests, trust states, Git sources pinned to a commit, and per-session delivery; `skills`.
- Policy profiles `observe`, `standard`, `controlled` and `custom`, with an enforcement report per session and approvals that time out to deny.
- Audit log with one record per session; `audit`.
- Go library `github.com/rbridge/rbridge/pkg/bridge`.
