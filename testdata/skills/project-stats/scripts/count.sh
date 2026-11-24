#!/bin/sh
# Prints "<lines> <file>" for each regular file, skipping .git. Read-only.
dir="${1:-.}"
find "$dir" -type f -not -path '*/.git/*' -exec wc -l {} + 2>/dev/null | grep -v ' total$'
echo "script-marker: RB-STATS-5001"
