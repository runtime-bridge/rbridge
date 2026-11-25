# Release notes from a Go program

A Go backend that needs release notes does not have to contain an agent. This program reads the Git history, hands it to an agent runtime already installed on the machine (Claude Code, OpenCode, any ACP agent) together with a `release-notes` skill, and writes the answer to a file. It uses the rbridge Go library in-process; no daemon is needed.

```sh
go run ./examples/go-release-notes -repo ~/src/app -version v1.4.0
go run ./examples/go-release-notes -repo ~/src/app -since v1.3.0 -provider claude-code -model haiku -v
```

| Flag | Default | Meaning |
|---|---|---|
| `-repo` | `.` | Git repository |
| `-since` | latest tag | Start of the range; without a tag, the whole history |
| `-version` | `Unreleased` | Heading for the notes |
| `-out` | `RELEASE_NOTES.md` | File to write, relative to the repository |
| `-provider`, `-model` | rbridge defaults | Which runtime and model to use |
| `-v` | off | Show the answer as it streams |

What it shows:

- **Skills travel with the application.** `skills/release-notes/SKILL.md` is embedded in the binary, added to rbridge's library at start and marked reviewed, because the program ships it. rbridge delivers it to whichever runtime runs the task.
- **The runtime only reads.** The task runs under the `observe` profile, so the runtime cannot change files or run commands; the program writes the result itself.
- **Commit messages carry the detail.** Each commit's body goes into the prompt, so the runtime does not need a shell to understand the changes.

The smoke test (`go test ./examples/go-release-notes/`) runs the program against a fake Claude Code and checks that the right commits reach the runtime.
