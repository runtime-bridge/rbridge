# Examples

Each example is a small program that hands real work to an agent runtime already installed on the machine, with a skill it ships itself. None of them contains an agent.

| Example | Language and interface | What it shows |
|---|---|---|
| [go-release-notes](go-release-notes) | Go, embedded library | A skill embedded in the binary, a read-only task, the program writing the result |
| [python-security-review](python-security-review) | Python SDK (`Rbridge`) | Structured findings from a JSON schema, a report, an exit code for CI |
| [ts-ops-check](ts-ops-check) | Node.js SDK (`Rbridge`) | Commands approved by the application's own rules, skill scripts following trust, the optional sandbox |

Each has a smoke test that runs it against rbridge's fake agent, in CI and locally after `make build fakeagent`. All three were also run against Claude Code.
