---
name: code-review
description: Review a source file against a short checklist and list concrete problems. Use when asked for a code review.
license: Apache-2.0
metadata:
  corpus: rbridge-compat
  version: "1.0.0"
---

# Code review

1. Read `references/checklist.md` from this skill directory first.
2. Review the file you are asked about against every item.
3. Answer with one line per problem: `<file>:<line> <problem>`.
4. End with the line `marker: RB-REVIEW-2001` and the reference marker from the checklist.
