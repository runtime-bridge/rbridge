# Skill compatibility

Results of `tests/integration` (`TestSkillCompatibility`) with the five skills in `testdata/skills`. Each skill carries markers, so a run shows whether the runtime read `SKILL.md`, read a reference file and ran a script. Model answers are not compared word for word.

"Discovered" comes from the runtime's own list of skills where it reports one (Claude Code does in its init event; OpenCode over ACP does not, so it shows n/a).

## Claude Code 2.1.331, two accounts, model haiku, 2026-11-25

Same binary, two accounts (`claude-code` on `~/.claude`, `claude-code-pro` started through its own wrapper). The basic turn, a real cancel and a run through the daemon over IPC also passed on both.

| Provider | Skill | Discovered | Instructions | Reference | Script | Output | Seconds |
|---|---|---|---|---|---|---|---|
| claude-code | doc-writer | yes | yes | n/a | n/a | ok | 17 |
| claude-code | code-review | yes | yes | yes | n/a | ok | 24 |
| claude-code | security-audit | yes | yes | yes | yes | ok | 26 |
| claude-code | report-builder | yes | yes | n/a | n/a | ok (valid JSON) | 11 |
| claude-code | project-stats | yes | n/a | n/a | yes | ok | 12 |
| claude-code-pro | doc-writer | yes | yes | n/a | n/a | ok | 30 |
| claude-code-pro | code-review | yes | yes | yes | n/a | ok | 16 |
| claude-code-pro | security-audit | yes | yes | yes | yes | ok | 27 |
| claude-code-pro | report-builder | yes | yes | n/a | n/a | ok (valid JSON) | 13 |
| claude-code-pro | project-stats | yes | n/a | n/a | yes | ok | 16 |

## OpenCode 1.19.6 with Ollama

Not measured: the local model never finished downloading. Skill discovery in OpenCode was checked with `opencode debug skill` instead (see the spike report).
