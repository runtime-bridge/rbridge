# Checklist

| Area | Look for |
|---|---|
| Injection | SQL built with string formatting, shell commands built from input (`shell=True`, `os.system`), template injection |
| Paths | File paths from input without normalising and checking they stay inside a base folder |
| Secrets | Keys, tokens or passwords in source, logs or error messages |
| Deserialisation | `pickle`, `yaml.load` without a safe loader, `eval` or `exec` on input |
| Authentication | Missing checks on sensitive routes, comparing secrets with `==` instead of a constant-time compare |
| Crypto | MD5 or SHA-1 for passwords, fixed IVs, `random` used for tokens |
| Web | Missing output escaping, open redirects, permissive CORS |
