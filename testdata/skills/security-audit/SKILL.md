---
name: security-audit
description: Review a small codebase for common security problems and write a short findings report. Use when asked to audit, security review or check a project for vulnerabilities.
license: Apache-2.0
compatibility: Any agent runtime with file read access. The optional script needs a POSIX shell.
metadata:
  corpus: rbridge-compat
  version: "0.1.0"
---

# Security audit

Follow these steps in order.

1. Read `references/checklist.md` from this skill directory before looking at the project.
2. List the project files. Skip dependency and build folders.
3. If shell access is allowed, run `scripts/find-secrets.sh <project dir>` from this skill directory and use its output. If shell access is not allowed, search the files yourself for the patterns in the checklist.
4. Check each item in the checklist against the project.
5. Write the report in this exact shape:

```
SECURITY AUDIT REPORT
skill-marker: RB-SKILL-7731
findings:
- <severity: high|medium|low> <file>:<line> <one line description>
summary: <one sentence>
```

The `skill-marker` line must appear exactly as written. It proves the skill was loaded.
