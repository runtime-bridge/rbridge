# Security review from Python

A security tool can bring its knowledge as a skill and let an agent runtime the user already has do the reasoning. `review.py` gives rbridge a `vuln-review` skill (instructions plus a checklist), runs the review read-only on whichever runtime rbridge picks, asks for the findings as JSON, and writes a Markdown report. The exit code makes it usable as a CI gate.

```sh
pip install git+https://github.com/runtime-bridge/rbridge-sdk-python
python3 examples/python-security-review/review.py                 # reviews sample/
python3 examples/python-security-review/review.py ~/src/app --runtime claude-code --model haiku --fail-on critical
```

`sample/app.py` is a small web service with deliberate flaws (SQL and command injection, path traversal, a hard-coded token, MD5 passwords) to try it on. A run with Claude Code reports each with its line, impact and fix.

| Option | Default | Meaning |
|---|---|---|
| `project` | `sample/` | Folder to review |
| `--out` | `SECURITY_REVIEW.md` | Report file |
| `--runtime`, `--model` | rbridge defaults | Which runtime and model |
| `--fail-on` | `high` | Exit with 2 when a finding is at this severity or worse; `never` to always exit 0 |

What it shows:

- **One call.** The whole review is a single `rb.run(...)` with the prompt, the workspace, the skill, the profile and a schema; the SDK starts the daemon when it is not running.
- **The application ships its skill.** `skills=` names the folder `skills/vuln-review`; the SDK adds it and trusts it as reviewed on first use, since the application wrote it.
- **Read-only by policy.** The `observe` profile lets the runtime read the code but not change or run anything.
- **Structured answers.** The findings come back in the shape of a JSON schema when the runtime supports it, and are found in the text otherwise.

`test_review.py` runs the script against a daemon whose runtime is rbridge's fake agent: `PYTHONPATH=<sdk>/src python3 -m unittest test_review` after `make build fakeagent`.
