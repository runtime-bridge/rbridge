# Changelog

## v1.0.0 (unreleased)

- An everyday API in both SDKs, in the shape of the familiar model SDKs with the runtime in place of the model: `Rbridge` (and `AsyncRbridge` in Python) with `run(prompt, runtime=...)`, `stream=True` to print the answer as it is written while still returning it (a function receives the chunks; `stream()` is the iterator), `chat=True` to make every run continue one conversation, a list of messages in one call, everything settable once on `Rbridge(...)`, `chat()` for separate conversations, `run_many` / `runMany` for several at once, `schema` for parsed JSON, skill folders by path, permission handling as one option, and a spending limit. The lower-level clients are unchanged underneath.

- Easier to use: a mistyped command, subcommand or provider gets a "did you mean" and the list of choices, `ls`, `rm` and `info` work as aliases, and `RBRIDGE_HOME` may be relative (such as `.rbridge`). `rbridge daemon start --exit-when-idle 30m` stops the daemon once nothing has used it for that long. The SDKs start a daemon when none is running (one that stops after 30 idle minutes; `start=False` turns this off), take a plain prompt as a task, and default the workspace to the current folder, resolving relative paths.

- `rbridge uninstall` lists the program and its data folders and removes them with `--yes` (and `--purge` for the data); for a copy that came with an SDK it names the package manager instead.

- Per-project installs: the Python wheels and npm packages for Linux, macOS and Windows carry rbridge, keep its data in the project's `.rbridge` folder, and can start their own daemon (`connect(start=True)`, `connect({ start: true })`). The Node.js package is now `@rbridge/sdk`.

- `rbridge init` creates rbridge's folders and a configuration file: a commented starter, or with `--config FILE` your own, checked before it is written. A different existing file is replaced only with `--force`, which keeps a backup. `--skills DIR` (with `--only`, `--exclude` and `--trust`) adds a folder of skills in the same step. Running it again is safe.

- Adding many skills at once: `rbridge skills add` finds every skill below the folders it is given (or in a Git repository, with `--subdir` repeatable), with `--only` and `--exclude` taking names or patterns such as `metric-*`, `--dry-run` to preview, and `--trust` applied to every skill added. One invalid skill does not stop the rest. `rbridge skills trust` and `rbridge skills remove` take several ids or patterns. The daemon gains `skills.add_many` (`POST /v1/skills/batch`), the Go library `AddSkills` and `AddSkillsFromGit`, and the SDKs `addSkills` and `add_skills`.

- Resource governor: with `execution.resource_ceiling_percent` set, the number of sessions allowed at once follows the machine's CPU and memory load, from every configured slot down to one at the ceiling, never zero. `rbridge doctor` shows the current reading.

- Claude Code's Skill tool counts as a read, so read-only profiles such as `observe` can load the skills rbridge delivers. Scripts a skill runs are still judged as commands.

- Examples: `go-release-notes` (embedded Go library), `python-security-review` (Python SDK) and `ts-ops-check` (Node.js SDK), each with its own skill and a smoke test in CI. The SDKs gain calls to add, trust and search skills.

- Optional OS sandbox on Linux (`--sandbox files|full`, `sandbox.mode`, `policy.custom.sandbox`): bubblewrap when it works, Docker otherwise. Only the workspace, the session folder and the runtime's own folders are writable; `full` also removes the network. Terminal commands run in the same sandbox. The enforcement notice, session record and audit log show the engine and mode, and a task that asks for a sandbox fails rather than running without one.

- Model servers: `ollama` is built in, and any Ollama or OpenAI-compatible server on this machine can be added with `integration: model-server`. They generate text only, refuse skills and MCP servers, report usage, and are used only when a task names them, never in place of an agent runtime.

- `resolution.fallback` and `resolution.allow_billing_class_change` now work: with fallback allowed, a task whose provider is unavailable or fails to start moves to the next provider of the same billing class, and the move is shown as a notice and recorded in the session and the audit log.

- Measured skill compatibility: each session records, per skill and runtime, the delivery mode, runtime version, turn outcomes and whether the runtime was seen opening the skill. `rbridge skills show` reports it for the installed version.

- Skill catalogs: `skills.catalogs` lists HTTP indexes of skills pinned to a Git commit and a digest (`specs/skill-catalog.md`). `rbridge skills search` and `rbridge skills add --catalog` use them, and an install is refused unless the files match the listed digest. `rbridge skills digest` computes the value for publishers.

- `rbridge skills import` adds the skills listed in a project's `skills-lock.json`, verifying each against the lock's content hash before installing it.

- Per-session skills for Gemini CLI, Codex and custom ACP agents with `skills_dir`: linked into the project's skill folder for the session, excluded from Git, removed afterwards and cleaned up after a crash. A project skill of the same name is never replaced.

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
