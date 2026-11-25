---
name: vuln-review
description: Reviews source code for security vulnerabilities and reports each finding with its location, impact and a fix.
license: Apache-2.0
---

# Security review

Review the code in the working directory for vulnerabilities an attacker could use.

1. Find the entry points first: HTTP handlers, CLI arguments, file and network input, environment variables.
2. Follow untrusted input to where it is used. Check it against `references/checklist.md`.
3. Report only problems you can point to in the code. Do not report style issues or missing tests.
4. For each finding give: the severity (`critical`, `high`, `medium`, `low`), the file and line, a short title, what an attacker could do, and the smallest fix.
5. Do not change any files. If the answer format is given as a JSON schema, follow it exactly.
