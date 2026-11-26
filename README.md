<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.png">
    <img src="docs/assets/logo-light.png" alt="rbridge" width="220">
  </picture>
</p>

# rbridge

rbridge lets software on your machine use the agent runtimes you already have installed, such as Claude Code and OpenCode, through one interface, and carries Agent Skills to them in the way each runtime expects.

It does not run its own agent loop and it never touches your provider credentials. Each runtime signs in through its own flow; rbridge starts the runtime you installed and talks to it over a documented protocol (ACP, or Claude Code's headless stream-json mode).

## Status

Early development, not tagged yet. What works today:

- `rbridge providers` and `rbridge doctor` find installed runtimes and report version, path, integration and sign-in mode (when it can be known without reading credentials).
- `rbridge run` and `rbridge chat` hand a task to Claude Code or OpenCode, stream the work as it happens, ask you before the runtime does something that needs permission, and stop cleanly on Ctrl+C.
- `rbridge skills` keeps a library of Agent Skills with a digest and a trust state for each, and delivers them to a session without touching your project or your own skill folders.
- `rbridge sessions` lists, shows, resumes and cancels sessions.
- `github.com/runtime-bridge/rbridge/pkg/bridge` does the same from Go code, in your own process.

- Policy profiles decide what a runtime may do, and `rbridge audit` keeps one record per session.

- `rbridge daemon` serves several applications over a local socket and, if enabled, local HTTP.

Codex, Gemini CLI and any ACP agent are supported too, with SDKs for Node.js and Python. See the roadmap below.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/runtime-bridge/rbridge/main/scripts/install.sh | sh
```

The script verifies the release checksum (and the cosign signature when cosign is installed) before installing to `~/.local/bin`. With Go 1.25 or later: `go install github.com/runtime-bridge/rbridge/cmd/rbridge@latest`.

That is the global install: one rbridge on your `PATH` for the whole machine, with its data in your user folders. The other way is per project: the Python and Node.js SDK packages carry rbridge for Linux, macOS and Windows, so `pip install rbridge` in a virtual environment or `npm install @rbridge/sdk` in a project brings a copy that lives only there, keeps its data in the project's `.rbridge` folder, and goes away with `pip uninstall` or `npm uninstall`. Inside such a project its copy is used, never the global one.

`rbridge uninstall` lists the program and every data folder with their sizes and changes nothing; `--yes` removes the program, and `--purge --yes` the configuration, skills, sessions and audit log as well. For a copy that came with an SDK it points you at pip or npm instead.

The full documentation, with examples for the CLI and every SDK, is at **[runtime-bridge.github.io](https://runtime-bridge.github.io/docs/)**. New here? Follow [docs/getting-started.md](docs/getting-started.md). Reference: [configuration](docs/configuration.md), [threat model](docs/threat-model.md), [performance](docs/performance.md), [releasing](docs/releasing.md).

Or from a checkout:

```sh
make build
./bin/rbridge doctor
```

## Usage

```sh
$ rbridge providers
PROVIDER     STATUS     VERSION  INTEGRATION      AUTH     PATH
claude-code  available  2.1.331  claude-headless  unknown  /home/me/.local/bin/claude
opencode     available  1.19.6   acp              unknown  /home/me/.local/bin/opencode

$ rbridge run --workspace ~/src/app "Summarise what this project does"
$ rbridge run --provider opencode --model ollama/qwen3:8b --quiet "List the TODO comments"
$ echo "Explain main.go" | rbridge run --json
$ rbridge chat --provider claude-code       # a normal conversation, streamed; /exit to leave
$ rbridge run --model latest "..."          # the runtime's newest flagship; auto = its own recommended choice
```

A mistyped command or runtime gets a suggestion (`rbridge skill list` answers "did you mean skills?"), and `ls`, `rm` and `info` work as aliases.

Without `--provider`, rbridge picks the first available runtime in `providers.order` that has every capability given with `--require`. A named runtime is never swapped for another unless `resolution.fallback: allowed`, and even then only for one with the same billing class (subscription, API key or local) unless `resolution.allow_billing_class_change` is set; every move is reported and audited. A runtime that needs permission for an action asks through rbridge: on a terminal you are asked, otherwise the request is denied unless you pass `--approve allow`.

Every command accepts `--json`. Exit codes are stable: `2` invalid input, `10` provider not found, `11` provider unavailable, `12` sign-in required, `13` no provider has the required capabilities, `20` to `24` skill problems, `40` timeout, `41` the runtime failed, `42` cancelled, `50` session not found.

### Skills

```sh
$ rbridge skills add ./skills/security-audit        # copied into the library, trust: unreviewed
$ rbridge skills show security-audit                # read it before trusting it
$ rbridge skills trust security-audit reviewed
$ rbridge run --skill security-audit "Audit this project"
```

A folder of skills goes in with one command: rbridge finds every folder with a `SKILL.md` below it. Pick with names or patterns, preview first, and trust them in the same step when they are your own:

```sh
$ rbridge skills add ./skills                                  # every skill found under ./skills
$ rbridge skills add ./skills --exclude 'draft-*' --dry-run    # see what would be added
$ rbridge skills add ./skills --only metric-discovery,metric-verification --trust reviewed
$ rbridge skills trust 'metric-*' reviewed                     # trust and remove also take several ids or patterns
```

A Git repository works the same way: `--subdir` may be repeated, and without it every skill in the repository is found. One skill failing to validate does not stop the others; each is reported, and the command exits non-zero if any failed.

Skills use the [Agent Skills](https://agentskills.io/specification) format unchanged. Each installed skill has a SHA-256 digest of its files; if the files change, rbridge refuses to use it until it is added again. Trust states:

| Trust | Instructions delivered | Scripts delivered |
|---|---|---|
| unreviewed | no (unless `skills.allow_unreviewed`) | no |
| reviewed, trusted | yes | yes; running them still goes through approvals |
| restricted | yes | no |
| blocked | no | no |

For Claude Code, skills go into a per-session directory passed with `--add-dir`. For OpenCode, rbridge writes a per-session config file listing the skills and passes it with `OPENCODE_CONFIG`, which OpenCode merges with your own configuration. Either way the directory is removed when the session ends. Gemini CLI and Codex have no session-scoped skill path, so rbridge links the skills into the project's `.agents/skills` for the session only: it refuses if the project already has a skill of that name, adds the link to `.git/info/exclude`, and removes it when the session ends (or, after a crash, before the next session in that project). Custom ACP agents get the same with `skills_dir` in their configuration. Git sources must be pinned to a commit: `rbridge skills add https://github.com/org/skills --rev <commit> --subdir security-audit`.

Teams can publish a skill catalog (`specs/skill-catalog.md`): a JSON index of skills pinned to a Git commit and a digest. rbridge reads only the catalogs listed in `skills.catalogs`; `rbridge skills search <words>` lists matches and `rbridge skills add --catalog team:security-audit` installs one only if its files match the listed digest.

`rbridge skills show <id>` reports measured compatibility for the installed version: for each runtime it was used with, the runtime version, delivery mode, sessions, and whether the runtime was seen opening the skill (`works`) or only had it available (`delivered`). Nothing is taken from the skill's own claims.

Projects that use the `skills` installer keep a `skills-lock.json`. `rbridge skills import` reads it and adds each listed skill to the library: from the copy installed under `.agents/skills` when its files match the lock's hash, otherwise from its Git source pinned to the locked commit (`--no-fetch` turns that off), checked against the same hash. Entries that cannot be verified are skipped with the reason, and imported skills start unreviewed.

To use a skill with a runtime started by hand, place it in the runtime's own folder: `rbridge skills deliver security-audit --provider claude-code --permanent` (user folder) or add `--project .` (project folder, also added to `.git/info/exclude`). rbridge tracks what it placed, keeps it in step when trust changes, removes it when the skill is blocked or removed, and never replaces a folder it did not create. `rbridge skills deliveries` lists them and `rbridge skills undeliver <id>` removes them.

### Policy and audit

Every task runs under a profile (`--profile`, default `standard`):

| Profile | Reads | Edits in the workspace | Commands, fetches, deletes |
|---|---|---|---|
| observe | allowed | denied | denied |
| standard | allowed | allowed | asked |
| controlled | allowed | asked | asked |
| custom | from `policy.custom.rules` in config | | |

Writes outside the workspace are always denied, delivered skills are read-only, and a reviewed skill's scripts are asked about each time. With `observe`, Claude Code also has its write, shell and web tools removed. A request that nobody answers within `policy.approval_timeout` is denied.

ACP agents are offered terminals run by rbridge whenever the profile does not deny commands. Each command goes through the profile, runs inside the workspace with the session's filtered environment, is killed after `execution.command_timeout`, keeps at most `execution.max_output_bytes` of output, and is killed with its children when the session ends. An agent's own built-in shell still relies on the agent asking permission.

By default rbridge does not sandbox the runtime. With `--sandbox files` (or `sandbox.mode` in config) the runtime, and every command rbridge runs for it, starts in an OS sandbox where only the workspace, the session folder and the runtime's own state folders are writable; `--sandbox full` also removes all network access, so it suits runtimes on a local model, not cloud ones. rbridge uses bubblewrap when it works and Docker otherwise, on Linux only; a task that asks for a sandbox fails rather than running without one. The sandbox limits writes and network, not reads: a runtime still reads its own sign-in files. `rbridge doctor` shows which engine is available.

Each session starts with a notice saying which controls rbridge enforces, which rely on the runtime asking permission, and which are not enforced (network is enforced only in a full sandbox). Set `policy.require_enforced: [network]` to refuse tasks whose profile wants something rbridge cannot enforce. `rbridge audit` lists one record per session: profile, approvals and gaps, with no prompts or outputs.

### Go

```go
b, err := bridge.New(ctx, bridge.Options{})
if err != nil {
	return err
}
res, err := b.Run(ctx, bridge.Task{
	Workspace: "/home/me/project",
	Prompt:    bridge.Text("Review this project for security issues."),
	Skills:    []string{"security-audit"},
}, bridge.OnEvent(func(e bridge.Event) { log.Println(e.Type) }))
```

`bridge.Open` starts a multi-turn session; `WithApprover` decides permission requests (the default denies them).

### Node.js and Python

The SDKs have the shape of the familiar model SDKs, with the runtime in place of the model, and start the daemon themselves:

```python
from rbridge import Rbridge                        # pip install rbridge

rb = Rbridge(runtime="claude-code")
print(rb.run("Summarise what this project does").text)

bot = Rbridge(runtime="claude-code", chat=True, stream=True)
bot.run("What does main.go do?")                   # printed as it is written
bot.run("Now add tests for it")                    # same conversation
```

```js
import { Rbridge } from "@rbridge/sdk";            // npm install @rbridge/sdk

const rb = new Rbridge({ runtime: "claude-code" });
console.log((await rb.run("Summarise what this project does")).text);
```

`model` takes `latest`, `auto` or a model name, and `response.model` says which one ran. `run_many` / `runMany` runs prompts side by side and never fails for lack of room: runs queue until rbridge can take them, `schema` returns parsed JSON, `skills` takes the folders your program ships, `approve` decides permission requests and `max_cost_usd` caps spending. See the [Node.js](https://github.com/runtime-bridge/rbridge-sdk-ts) and [Python](https://github.com/runtime-bridge/rbridge-sdk-python) SDKs.

### Daemon

Several applications can share one rbridge through an optional daemon:

```sh
$ rbridge daemon start      # IPC socket on; set daemon.http.enabled for local HTTP
$ rbridge daemon start --exit-when-idle 30m   # stops by itself once nothing uses it (what the SDKs do)
$ rbridge daemon status
$ rbridge daemon stop
```

From Go, `bridge.Connect` returns a client with the same methods as the embedded `Bridge`, so moving between the two is a one-line change:

```go
var c bridge.Client
c, err := bridge.Connect(ctx, bridge.ConnectOptions{Client: "my-app"})
res, err := c.Run(ctx, task, bridge.WithApprover(decide))
```

The HTTP API listens on 127.0.0.1 only, needs the token from `daemon.token` in the runtime directory, refuses browser origins that are not listed in `daemon.http.allowed_origins`, and streams events as Server-Sent Events with replay. Each application names itself with `X-Rbridge-Client` (HTTP) or in `initialize` (IPC) and sees only its own sessions. See `specs/http-api.yaml` and `specs/ipc-protocol.md`.

### Examples

[`examples/`](examples) holds three small programs that use rbridge the way the project intends: a Go backend writing release notes, a Python security review and a Node.js operations check, the last two each a single `rb.run(...)`. Each ships its own skill and has a smoke test.

## Supported runtimes

| Runtime | How rbridge talks to it | Notes |
|---|---|---|
| Claude Code | `claude -p` with stream-json input and output | Uses your own Claude Code install and sign-in. Each account (`CLAUDE_CONFIG_DIR`) is its own provider: `claude-code`, `claude-code-<name>` |
| OpenCode | ACP (`opencode acp`) | Works with local models through Ollama |
| Codex | ACP through `codex-acp` | ChatGPT login or `CODEX_API_KEY` |
| Gemini CLI | ACP (`gemini --acp`) | API key by default; `providers.gemini-cli.allow_account_login` to opt in |
| Claude ACP adapter | ACP through `claude-agent-acp` | API key or cloud provider only |
| Any ACP agent | ACP, declared in config | Capabilities come from the agent's handshake |
| Ollama, or any OpenAI-compatible server on this machine | HTTP, text only | Used only when a task names it (`--provider ollama --model qwen3:8b`). No tools, files, commands or skills; rbridge keeps the conversation for the session. For agent work on a local model, use OpenCode with an Ollama model |

SDKs for other languages: [rbridge-sdk-ts](https://github.com/runtime-bridge/rbridge-sdk-ts) (Node.js) and [rbridge-sdk-python](https://github.com/runtime-bridge/rbridge-sdk-python). Other agents can use rbridge as an MCP server: `rbridge mcp`.

rbridge never bundles, installs or modifies a runtime, and never offers a provider's login. If you set an API key for a runtime (for example `ANTHROPIC_API_KEY`), that runtime bills it as usual; rbridge reports the mode as `api-key`.

## Configuration

Configuration is YAML, merged in this order, later layers winning:

1. built-in defaults
2. system: `/etc/rbridge/config.yaml` (macOS `/Library/Application Support/rbridge/config.yaml`, Windows `%ProgramData%\rbridge\config.yaml`)
3. user: `~/.config/rbridge/config.yaml` (macOS `~/Library/Application Support/rbridge/config.yaml`, Windows `%AppData%\rbridge\config.yaml`)
4. project: `.rbridge.yaml` in the current directory

The system file can list `locked` keys that the user and project layers cannot change. Unknown keys are an error.

`rbridge init` creates rbridge's folders and a commented starter configuration, or installs one you give it after checking it, and can add a folder of skills in the same step. Running it again changes nothing unless you ask:

```sh
rbridge init                                             # folders and a starter config.yaml
rbridge init --config ./rbridge.yaml                     # install this file (checked first)
rbridge init --config ./rbridge.yaml --force             # replace a different one; the old file is kept as config.yaml.bak
rbridge init --skills ./skills --trust reviewed          # add every skill in ./skills as well
```

```yaml
version: 1

providers:
  order: [opencode, claude-code]
  claude-code:
    executable: /opt/claude/bin/claude   # pin one installation
  custom:
    - id: my-agent
      launch:
        command: /usr/local/bin/my-agent
        args: [--acp]

execution:
  detect_cache_ttl: 10m
```

`RBRIDGE_HOME=/some/dir` keeps all of rbridge's configuration, data and cache under one directory.

## Development

```sh
make lint      # gofmt, go vet, staticcheck
make race      # tests with the race detector
make cover
make cross     # builds for Linux, macOS and Windows on amd64 and arm64
make licenses  # dependency licence check
```

Builds are CGO-free. Third-party dependencies: `go.yaml.in/yaml/v3` and `github.com/coder/acp-go-sdk`.

Unit tests run against fake runtimes. Tests against the real runtimes on your machine are opt-in, because they use your plan or local compute:

```sh
RBRIDGE_IT_PROVIDERS=claude-code,opencode \
RBRIDGE_IT_MODELS=claude-code=haiku,opencode=ollama/qwen3:8b \
RBRIDGE_IT_REPORT=docs/compatibility.md \
go test -tags integration -v -timeout 60m ./tests/integration/
```

## Roadmap

1. Core: detection, configuration, CLI (done)
2. Running tasks and sessions on Claude Code and OpenCode, streamed events, cancel, Go library (done)
3. Skills: validation, digests, trust states, delivery to each runtime (done)
4. Policy profiles and an audit log; first tagged release (done, not yet tagged)
5. Optional daemon with HTTP and IPC (done, not yet tagged)
6. More runtimes (Gemini CLI, Codex) and SDKs for other languages (done)
7. Production readiness: install script, docs, benchmarks (done); v1.0 waits for the first tagged releases

## License

Apache 2.0. See [LICENSE](LICENSE).
