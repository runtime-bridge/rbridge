#!/bin/sh
# Prints lines that look like hard-coded secrets. Read-only.
dir="${1:-.}"
grep -rnE "(password|secret|api_key|token)[[:space:]]*=[[:space:]]*['\"][^'\"]+['\"]|AKIA[0-9A-Z]{16}" "$dir" \
  --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=vendor 2>/dev/null || true
echo "script-marker: RB-SCRIPT-9150"
