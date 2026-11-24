# rbridge

rbridge lets software on your machine use the agent runtimes you already have installed, such as Claude Code and OpenCode, through one interface, and carries Agent Skills to them in the way each runtime expects.

It does not run its own agent loop and it never touches your provider credentials. Each runtime signs in through its own flow; rbridge starts the runtime you installed and talks to it over a documented protocol (ACP, or Claude Code's headless stream-json mode).

## Status

Early development, not tagged yet. What works today:

- `rbridge providers` and `rbridge doctor` find installed runtimes and report version, path, integration and sign-in mode (when it can be known without reading credentials).
- `rbridge run` and `rbridge chat` hand a task to Claude Code or OpenCode, stream the work as it happens, ask you before the runtime does something that needs permission, and stop cleanly on Ctrl+C.
- `rbridge skills` keeps a library of Agent Skills with a digest and a trust state for each, and delivers them to a session without touching your project or your own skill folders.
- `rbridge sessions` lists, shows, resumes and cancels sessions.
- `github.com/rbridge/rbridge/pkg/bridge` does the same from Go code, in your own process.

- Policy profiles decide what a runtime may do, and `rbridge audit` keeps one record per session.

A daemon with HTTP and IPC and more runtimes come next. See the roadmap below.

## Install

Requires Go 1.25 or later.

```sh
go install github.com/rbridge/rbridge/cmd/rbridge@latest
```

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
$ rbridge chat --provider claude-code
```

Without `--provider`, rbridge picks the first available runtime in `providers.order` that has every capability given with `--require`. A runtime that needs permission for an action asks through rbridge: on a terminal you are asked, otherwise the request is denied unless you pass `--approve allow`.

Every command accepts `--json`. Exit codes are stable: `2` invalid input, `10` provider not found, `11` provider unavailable, `12` sign-in required, `13` no provider has the required capabilities, `20` to `24` skill problems, `40` timeout, `41` the runtime failed, `42` cancelled, `50` session not found.

### Skills

```sh
$ rbridge skills add ./skills/security-audit        # copied into the library, trust: unreviewed
$ rbridge skills show security-audit                # read it before trusting it
$ rbridge skills trust security-audit reviewed
$ rbridge run --skill security-audit "Audit this project"
```

Skills use the [Agent Skills](https://agentskills.io/specification) format unchanged. Each installed skill has a SHA-256 digest of its files; if the files change, rbridge refuses to use it until it is added again. Trust states:

| Trust | Instructions delivered | Scripts delivered |
|---|---|---|
| unreviewed | no (unless `skills.allow_unreviewed`) | no |
| reviewed, trusted | yes | yes; running them still goes through approvals |
| restricted | yes | no |
| blocked | no | no |

For Claude Code, skills go into a per-session directory passed with `--add-dir`. For OpenCode, rbridge writes a per-session config file listing the skills and passes it with `OPENCODE_CONFIG`, which OpenCode merges with your own configuration. Either way the directory is removed when the session ends. Git sources must be pinned to a commit: `rbridge skills add https://github.com/org/skills --rev <commit> --subdir security-audit`.

### Policy and audit

Every task runs under a profile (`--profile`, default `standard`):

| Profile | Reads | Edits in the workspace | Commands, fetches, deletes |
|---|---|---|---|
| observe | allowed | denied | denied |
| standard | allowed | allowed | asked |
| controlled | allowed | asked | asked |
| custom | from `policy.custom.rules` in config | | |

Writes outside the workspace are always denied, delivered skills are read-only, and a reviewed skill's scripts are asked about each time. With `observe`, Claude Code also has its write, shell and web tools removed. A request that nobody answers within `policy.approval_timeout` is denied.

rbridge does not sandbox the runtime. Each session starts with a notice saying which controls rbridge enforces, which rely on the runtime asking permission, and which are not enforced (network is never enforced). Set `policy.require_enforced: [network]` to refuse tasks whose profile wants something rbridge cannot enforce. `rbridge audit` lists one record per session: profile, approvals and gaps, with no prompts or outputs.

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

## Supported runtimes

| Runtime | How rbridge talks to it | Notes |
|---|---|---|
| Claude Code | `claude -p` with stream-json input and output | Uses your own Claude Code install and sign-in |
| OpenCode | ACP (`opencode acp`) | Works with local models through Ollama |
| Any ACP agent | ACP, declared in config | Capabilities come from the agent's handshake |

rbridge never bundles, installs or modifies a runtime, and never offers a provider's login. If you set an API key for a runtime (for example `ANTHROPIC_API_KEY`), that runtime bills it as usual; rbridge reports the mode as `api-key`.

## Configuration

Configuration is YAML, merged in this order, later layers winning:

1. built-in defaults
2. system: `/etc/rbridge/config.yaml` (macOS `/Library/Application Support/rbridge/config.yaml`, Windows `%ProgramData%\rbridge\config.yaml`)
3. user: `~/.config/rbridge/config.yaml` (macOS `~/Library/Application Support/rbridge/config.yaml`, Windows `%AppData%\rbridge\config.yaml`)
4. project: `.rbridge.yaml` in the current directory

The system file can list `locked` keys that the user and project layers cannot change. Unknown keys are an error.

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
5. Optional daemon with HTTP and IPC
6. More runtimes (Gemini CLI, Codex) and SDKs for other languages

## License

Apache 2.0. See [LICENSE](LICENSE).
