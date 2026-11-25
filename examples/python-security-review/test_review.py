"""Smoke test: runs the review against an rbridge daemon whose only runtime is
the fake ACP agent. RBRIDGE_BIN and RBRIDGE_FAKE_AGENT point at the binaries
(default: this repository's bin folder, from make build fakeagent)."""

import json
import os
import shutil
import signal
import subprocess
import tempfile
import time
import unittest

import rbridge

import review

HERE = os.path.dirname(os.path.abspath(__file__))
BIN = os.path.normpath(os.path.join(HERE, "..", "..", "bin"))
RBRIDGE_BIN = os.environ.get("RBRIDGE_BIN", os.path.join(BIN, "rbridge"))
FAKE_AGENT = os.environ.get("RBRIDGE_FAKE_AGENT", os.path.join(BIN, "rbridge-fakeagent"))


class ReviewTest(unittest.TestCase):
    def setUp(self):
        self.home = tempfile.mkdtemp(prefix="rbex-")
        os.makedirs(os.path.join(self.home, "config"))
        with open(os.path.join(self.home, "config", "config.yaml"), "w") as f:
            f.write("\n".join([
                "version: 1",
                "providers:",
                "  order: [fake]",
                "  custom:",
                "    - id: fake",
                f"      launch: {{ command: {json.dumps(FAKE_AGENT)} }}",
                "      skills_dir: .agents/skills",
                "",
            ]))
        env = dict(os.environ, RBRIDGE_HOME=self.home)
        self.proc = subprocess.Popen([RBRIDGE_BIN, "daemon", "run"], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(100):
            if os.path.exists(os.path.join(self.home, "run", "daemon.json")):
                break
            time.sleep(0.1)
        self.project = os.path.join(self.home, "project")
        shutil.copytree(os.path.join(HERE, "sample"), self.project)

    def tearDown(self):
        self.proc.send_signal(signal.SIGTERM)
        self.proc.wait(timeout=30)
        shutil.rmtree(self.home, ignore_errors=True)

    def test_review_writes_a_report(self):
        out = os.path.join(self.home, "report.md")
        events = []
        with rbridge.connect(client="security-review", env={"RBRIDGE_HOME": self.home}) as c:
            code = review.review(self.project, out, client=c)
            skills = {s["id"]: s for s in c.skills()}
            sessions = c.sessions()
        self.assertEqual(code, 0)
        self.assertEqual(skills["vuln-review"]["trust"], "reviewed")
        self.assertTrue(sessions and sessions[0]["skills"][0]["skill_id"] == "vuln-review")
        with open(out) as f:
            text = f.read()
        # The fake agent echoes the prompt instead of reviewing.
        self.assertIn("# Security review", text)
        self.assertIn("Review this project for security vulnerabilities.", text)
        # The project-link delivery is gone after the session.
        self.assertFalse(os.path.exists(os.path.join(self.project, ".agents", "skills", "vuln-review")))

    def test_findings_and_exit_code(self):
        findings = [{"severity": "low", "file": "a.py", "title": "t", "impact": "i", "fix": "f"},
                    {"severity": "critical", "file": "b.py", "line": 3, "title": "SQL injection", "impact": "i", "fix": "f"}]
        self.assertEqual(review.findings_from({"structured": {"findings": findings}}), findings)
        self.assertEqual(review.findings_from({"output": "Here:\n```json\n" + json.dumps({"findings": []}) + "\n```"}), [])
        self.assertIsNone(review.findings_from({"output": "no json"}))
        text = review.report("/p", findings, "", "claude-code")
        self.assertLess(text.index("CRITICAL"), text.index("LOW"))
        self.assertIn("`b.py:3`", text)


if __name__ == "__main__":
    unittest.main()
