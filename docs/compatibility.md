# Skill compatibility

Results of `tests/integration` (`TestSkillCompatibility`) with the five skills in `testdata/skills`. Each skill carries markers, so a run shows whether the runtime read `SKILL.md`, read a reference file and ran a script. Model answers are not compared word for word.

"Discovered" comes from the runtime's own list of skills where it reports one (Claude Code does in its init event; OpenCode over ACP does not, so it shows n/a).

## Claude Code 2.1.331, model haiku, 2026-11-24

| Skill | Discovered | Instructions | Reference | Script | Output | Seconds |
|---|---|---|---|---|---|---|
| doc-writer | yes | yes | n/a | n/a | ok | 15 |
| code-review | yes | yes | yes | n/a | ok | 45 |
| security-audit | yes | yes | yes | yes | ok | 37 |
| report-builder | yes | yes | n/a | n/a | ok (valid JSON) | 19 |
| project-stats | yes | n/a | n/a | yes | ok | 29 |

## OpenCode 1.19.6 with Ollama

Not measured yet: the local model was still downloading.
