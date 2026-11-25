# Changelog

## v1.0.0 (unreleased)

- `rbridge skills deliver --permanent [--project DIR]` places a skill in a runtime's own skill folder and tracks it; `skills undeliver` and `skills deliveries` manage them. Trust changes update them, blocking or removing a skill removes them, and project deliveries are added to `.git/info/exclude`.

- ACP terminals served by rbridge: commands go through the policy profile, run in the workspace with the filtered environment, and are bounded by `execution.command_timeout` and `execution.max_output_bytes`; all are killed when the session ends. The enforcement notice lists them as enforced.
- Native runtime modes and settings for ACP agents: `run --mode`, chat `/modes`, `/mode`, `/options`, `/set`, daemon `sessions.modes`, `sessions.set_mode`, `sessions.options`, `sessions.set_option`, and the same in the Go, Node.js and Python clients. New event `mode.updated`.
- Questions a runtime asks the user (forms and URLs) reach the caller: a prompt on the terminal, `bridge.WithInput`, `onInput` and `on_input` in the SDKs, and `input.required` with `inputs.answer` in the daemon. Without a handler they are declined.
- An agent answering with a different ACP protocol version fails with `provider_protocol_error`.
- A workspace reached through a symbolic link (such as macOS temp folders under `/var`) is resolved before policy applies, so the runtime's own files in it are no longer refused as outside the workspace.

- Several Claude Code accounts on one machine: each becomes provider `claude-code-<name>`, found from `claude-<name>` wrapper scripts and `~/.claude-<name>` directories or declared in config, and started through its wrapper when it has one. `claude-code` is always `~/.claude` and no longer follows the caller's `CLAUDE_CONFIG_DIR`.

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
- Go library `github.com/runtime-bridge/rbridge/pkg/bridge`.
