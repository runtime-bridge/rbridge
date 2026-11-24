#!/bin/sh
# Fails when a module linked into rbridge has no licence file with a
# permissive licence (Apache 2.0, MIT, BSD, ISC).
set -eu
main=$(go list -m)
mods=$(go list -deps -f '{{with .Module}}{{.Path}}|{{.Dir}}{{end}}' "${1:-./cmd/...}" | sort -u)
status=0
IFS='
'
for entry in $mods; do
  path=${entry%%|*}
  dir=${entry#*|}
  [ "$path" = "$main" ] && continue
  lic=$(ls "$dir" | grep -iE '^(licen[cs]e|copying)' | head -n 1 || true)
  if [ -z "$lic" ]; then
    echo "no licence file: $path"
    status=1
  elif grep -qiE 'Apache License|MIT License|Permission is hereby granted, free of charge|Redistribution and use in source and binary forms|ISC License' "$dir/$lic"; then
    echo "ok: $path ($lic)"
  else
    echo "unrecognised licence: $path ($lic)"
    status=1
  fi
done
exit $status
