# Checklist

- Hard-coded secrets: strings that look like API keys, passwords or tokens (`password =`, `secret`, `api_key`, `AKIA`).
- Command injection: user input passed to a shell (`os.system`, `subprocess` with `shell=True`, `exec.Command("sh", "-c", ...)`).
- SQL injection: queries built with string formatting or concatenation.
- Path traversal: file paths built from user input without cleaning.
- Weak crypto: `md5` or `sha1` used for passwords.

Reference marker: RB-REF-4402. Include it in the summary line.
