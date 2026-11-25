"""Security review of a project by an agent runtime installed on this machine.

The script holds no agent of its own. It gives rbridge a vuln-review skill and
the project, lets whichever runtime rbridge picks do the review read-only, and
turns the findings into a report. Start the daemon first: rbridge daemon start.
"""

import argparse
import json
import os
import re
import sys

import rbridge

HERE = os.path.dirname(os.path.abspath(__file__))
SEVERITIES = ["critical", "high", "medium", "low"]

SCHEMA = {
    "type": "object",
    "required": ["findings"],
    "properties": {
        "findings": {
            "type": "array",
            "items": {
                "type": "object",
                "required": ["severity", "file", "title", "impact", "fix"],
                "properties": {
                    "severity": {"type": "string", "enum": SEVERITIES},
                    "file": {"type": "string"},
                    "line": {"type": "integer"},
                    "title": {"type": "string"},
                    "impact": {"type": "string"},
                    "fix": {"type": "string"},
                },
            },
        }
    },
}


def findings_from(result):
    """Structured output when the runtime supports it, otherwise JSON found in
    the text. None means the answer was not in the expected shape."""
    data = result.get("structured")
    if data is None:
        text = result.get("output", "")
        m = re.search(r"\{.*\}", text, re.S)
        if m:
            try:
                data = json.loads(m.group(0))
            except ValueError:
                data = None
    if isinstance(data, dict) and isinstance(data.get("findings"), list):
        return data["findings"]
    return None


def report(project, findings, raw, provider):
    lines = ["# Security review", "", f"Project: `{project}`, reviewed by {provider} through rbridge.", ""]
    if findings is None:
        lines += ["The runtime did not answer in the expected format. Its answer:", "", raw.strip(), ""]
        return "\n".join(lines)
    if not findings:
        lines += ["No findings.", ""]
    order = {s: i for i, s in enumerate(SEVERITIES)}
    for f in sorted(findings, key=lambda f: order.get(f.get("severity"), 9)):
        where = f.get("file", "?") + (f":{f['line']}" if f.get("line") else "")
        lines += [f"## {f.get('severity', '?').upper()}: {f.get('title', '')}", "", f"`{where}`", "",
                  f"**Impact:** {f.get('impact', '')}", "", f"**Fix:** {f.get('fix', '')}", ""]
    return "\n".join(lines)


def review(project, out, provider=None, model=None, fail_on="high", client=None):
    """Runs the review and writes the report. Returns the process exit code."""
    project = os.path.abspath(project)
    own = client is None
    c = client or rbridge.connect(client="security-review")
    try:
        # The application ships this skill, so it can vouch for it.
        c.add_skill(os.path.join(HERE, "skills", "vuln-review"))
        c.set_skill_trust("vuln-review", "reviewed")
        task = {
            "workspace": project,
            "prompt": "Review this project for security vulnerabilities.",
            "skills": ["vuln-review"],
            # Read-only: the runtime may look at the code but not change it or run it.
            "profile": "observe",
            "output_schema": SCHEMA,
        }
        if provider:
            task["provider"] = provider
        result = c.run(task, model=model, on_event=lambda e: e["type"] == "message.delta" and sys.stderr.write("."))
    finally:
        if own:
            c.close()
    sys.stderr.write("\n")
    findings = findings_from(result)
    with open(out, "w") as f:
        f.write(report(project, findings, result.get("output", ""), result.get("provider", "a runtime")))
    if findings is None:
        print(f"wrote {out}; the answer was not structured, read it there")
        return 0
    counts = {s: sum(1 for f in findings if f.get("severity") == s) for s in SEVERITIES}
    print(f"wrote {out}: " + ", ".join(f"{n} {s}" for s, n in counts.items() if n) if findings else f"wrote {out}: no findings")
    limit = SEVERITIES.index(fail_on) if fail_on in SEVERITIES else -1
    return 2 if any(counts[s] for s in SEVERITIES[: limit + 1]) else 0


def main():
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("project", nargs="?", default=os.path.join(HERE, "sample"))
    p.add_argument("--out", default="SECURITY_REVIEW.md")
    p.add_argument("--provider")
    p.add_argument("--model")
    p.add_argument("--fail-on", default="high", choices=SEVERITIES + ["never"],
                   help="exit with 2 when a finding is at this severity or worse")
    a = p.parse_args()
    try:
        sys.exit(review(a.project, a.out, a.provider, a.model, a.fail_on))
    except rbridge.RbridgeError as e:
        print(f"review failed: {e}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
