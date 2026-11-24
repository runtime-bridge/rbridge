# rbridge

rbridge lets software on your machine use the agent runtimes you already have installed, such as Claude Code and OpenCode, through one interface, and carries Agent Skills to them in the way each runtime expects.

It does not run its own agent loop and it never touches your provider credentials. Each runtime signs in through its own flow; rbridge starts the runtime you installed and talks to it over a documented protocol (ACP, or Claude Code's headless stream-json mode).

## Status

Early development. What works today:

- `rbridge providers` finds installed runtimes and reports version, path, integration and sign-in mode (when it can be known without reading credentials).
- `rbridge providers show <id>` lists capabilities, where each value came from, native skill locations and known limits.
- `rbridge doctor` checks configuration, rbridge's own directories and every runtime.

Running tasks, sessions, skills management and the Go library come next. See the roadmap below.

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
```

Every command accepts `--json`. Detection results are cached for ten minutes; `--refresh` detects again.

Exit codes are stable: `10` provider not found, `11` provider unavailable, `13` no provider has the required capabilities, `2` invalid input. `rbridge doctor` exits `1` when a check fails.

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

Builds are CGO-free. The only third-party dependency is `go.yaml.in/yaml/v3`.

## Roadmap

1. Core: detection, configuration, CLI (this release)
2. Running tasks and sessions on Claude Code and OpenCode, streamed events, cancel, Go library
3. Skills: validation, digests, trust states, delivery to each runtime
4. Policy profiles, approvals and an audit log; first tagged release
5. Optional daemon with HTTP and IPC
6. More runtimes (Gemini CLI, Codex) and SDKs for other languages

## License

Apache 2.0. See [LICENSE](LICENSE).
