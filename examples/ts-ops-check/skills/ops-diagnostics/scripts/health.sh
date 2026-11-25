#!/bin/sh
# Prints "name status http_code seconds" for each service in a services.json.
set -u
file=${1:-services.json}
python3 - "$file" <<'PY'
import json, sys, time, urllib.request, urllib.error
for s in json.load(open(sys.argv[1])):
    start = time.time()
    try:
        with urllib.request.urlopen(s["url"], timeout=5) as r:
            code = r.status
    except urllib.error.HTTPError as e:
        code = e.code
    except Exception:
        code = 0
    status = "up" if 200 <= code < 300 else "down"
    print(f'{s["name"]} {status} {code} {time.time() - start:.2f}')
PY
