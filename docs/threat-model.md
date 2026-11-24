# Threat model

rbridge runs on a developer's machine, starts agent runtimes the user installed, and lets local software give them tasks. It is not a sandbox.

## Assets

- The user's files, especially outside the task's workspace.
- Provider credentials and sessions (Claude, OpenAI, Google accounts, API keys).
- Secrets in the environment.
- The user's plan usage and API spend.

## Trust boundaries

| Boundary | What crosses it | Controls |
|---|---|---|
| Application to rbridge | Tasks, approvals | CLI and library run as the user. IPC socket is 0600 in a 0700 directory. HTTP: loopback only, token rewritten at each start, Host and Origin checks, body and rate limits, per-client session ownership |
| rbridge to runtime | Prompts, skills, environment, file operations | Allowlisted environment; credentials never read or stored; runtime binaries used as installed; skills delivered per session, scripts withheld unless trusted; ACP file reads and writes confined to the workspace with symlink checks and policy |
| Runtime to machine | Tool calls | Policy profile on every permission request; tools removed outright in `observe` for Claude Code; process tree killed on timeout, cancel and close. Beyond that, only the runtime's own rules and the OS apply |

## Threats and mitigations

| Threat | Mitigation | Residual risk |
|---|---|---|
| Malicious skill instructions | Skills start unreviewed and are refused; `skills show` before trusting; digest check before every run | A trusted skill can still talk a runtime into harmful actions; policy and approvals are the backstop |
| Malicious skill scripts | Scripts withheld unless reviewed or trusted; reviewed scripts asked about each run | A runtime can write and run its own code where policy allows commands |
| Skill changed after review | SHA-256 over the file tree, checked before every delivery; new content resets trust | None known |
| Prompt injection from project files | Treated as untrusted content; approvals for sensitive kinds; `observe` for untrusted projects | Depends on the runtime asking permission |
| Credential leakage | rbridge never opens credential files (tested); runtimes get an allowlisted environment; audit stores no prompts or tool input | A runtime may print secrets it can read in the workspace |
| Web page calling the local API | No token in the page; Origin refused; Host checked against DNS rebinding | Disabled by default |
| Another local user | Socket and files are owner-only | Root and the same user are trusted |
| Path traversal and symlink escape | Canonical paths and symlink resolution for every ACP file operation; skill symlinks must stay inside the skill | Runtime's own tools are outside rbridge's view |
| Runaway runtime | Task timeout, idle watchdog, concurrency limit, process tree kill | Resource use while running |
| Terms violation by design | No credential handling; API key required where providers' terms demand it (`claude-acp`, `gemini-cli` by default) | Terms can change; check `providers show` notes |
| Supply chain | Two permissive dependencies, licence check in CI, checksums and cosign signatures on releases, install script verifies before writing | Compromise of the release pipeline |

## Not covered

Network restrictions, CPU and memory limits, and containment of a runtime that acts without asking permission. Run untrusted work in a container or VM if those matter.

Report vulnerabilities as described in `SECURITY.md`.
