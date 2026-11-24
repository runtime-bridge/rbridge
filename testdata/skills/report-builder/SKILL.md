---
name: report-builder
description: Produce a JSON summary of a project's files using a fixed shape. Use when asked for a machine-readable project report.
license: Apache-2.0
metadata:
  corpus: rbridge-compat
  version: "1.0.0"
---

# Project report

1. List the files in the project.
2. Fill in the shape in `assets/shape.json` from this skill directory.
3. Answer with the JSON only, no prose. Set `"marker"` to `"RB-REPORT-4001"`.
