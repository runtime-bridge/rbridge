---
name: project-stats
description: Count lines of code per file by running the bundled script. Use when asked for project statistics.
license: Apache-2.0
compatibility: Needs a POSIX shell to run scripts/count.sh.
metadata:
  corpus: rbridge-compat
  version: "1.0.0"
---

# Project statistics

1. Run `scripts/count.sh <project dir>` from this skill directory.
2. Report the script output as a table of file and line count.
3. Include the `script-marker` line from the script output exactly.

If you cannot run scripts, say `scripts unavailable` and stop.
