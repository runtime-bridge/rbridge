// Smoke test against an rbridge daemon whose only runtime is the fake ACP
// agent. RBRIDGE_BIN and RBRIDGE_FAKE_AGENT point at the binaries (default:
// this repository's bin folder, from make build fakeagent).
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { Rbridge } from "@rbridge/sdk";
import { approve, check, verdict } from "./check.js";

const here = path.dirname(fileURLToPath(import.meta.url));
const bin = path.resolve(here, "..", "..", "bin");
const RBRIDGE = process.env.RBRIDGE_BIN ?? path.join(bin, "rbridge");
const FAKE = process.env.RBRIDGE_FAKE_AGENT ?? path.join(bin, "rbridge-fakeagent");

async function daemon(scenario) {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), "rbops-"));
  fs.mkdirSync(path.join(home, "config"));
  fs.writeFileSync(
    path.join(home, "config", "config.yaml"),
    ["version: 1", "providers:", "  order: [fake]", "  custom:", "    - id: fake",
      `      launch: { command: ${JSON.stringify(FAKE)}, env_passthrough: [RBRIDGE_FAKE_ACP_SCENARIO] }`,
      "      skills_dir: .agents/skills", ""].join("\n"),
  );
  const proc = spawn(RBRIDGE, ["daemon", "run"], { env: { ...process.env, RBRIDGE_HOME: home, RBRIDGE_FAKE_ACP_SCENARIO: scenario }, stdio: "ignore" });
  for (let i = 0; i < 100 && !fs.existsSync(path.join(home, "run", "daemon.json")); i++) await new Promise((r) => setTimeout(r, 100));
  const project = path.join(home, "project");
  fs.cpSync(path.join(here, "sample"), project, { recursive: true });
  return {
    project,
    env: { RBRIDGE_HOME: home },
    async stop() {
      proc.kill("SIGTERM");
      await new Promise((r) => proc.once("exit", r));
      fs.rmSync(home, { recursive: true, force: true });
    },
  };
}

test("approval rules", () => {
  const ask = (command, kind = "execute") => approve({ kind, title: "run " + command, input: { command } });
  assert.equal(ask("/tmp/s/skills/ops-diagnostics/scripts/health.sh services.json"), true);
  assert.equal(ask("sh scripts/health.sh"), true);
  assert.equal(ask("bash /x/skills/ops-diagnostics/scripts/health.sh /home/me/project/services.json"), true);
  assert.equal(ask("cd /x && scripts/health.sh"), false);
  assert.equal(ask("df -h"), true);
  assert.equal(ask("rm -rf /"), false);
  assert.equal(ask("df -h; rm -rf /"), false);
  assert.equal(ask("systemctl restart api"), false);
  assert.equal(approve({ kind: "edit", title: "Write x", input: {} }), false);
  assert.equal(approve({ kind: "read", title: "Read /proc/uptime", input: {} }), true);
});

test("verdict reads structured or embedded JSON", () => {
  const v = { status: "degraded", checks: [{ name: "api", status: "down" }] };
  assert.deepEqual(verdict(v), v);
  assert.deepEqual(verdict(null, "Result:\n" + JSON.stringify(v)), v);
  assert.equal(verdict(null, "no json"), undefined);
});

test("runs through the daemon with the skill delivered", async () => {
  const d = await daemon("echo");
  try {
    const rb = new Rbridge({ app: "ops-check", env: d.env });
    const res = await check({ dir: d.project, rb });
    assert.match(res.text, /Check the health of the services/);
    assert.equal(res.raw.skills[0].skill_id, "ops-diagnostics");
    assert.equal((await (await rb.client()).skills()).find((s) => s.id === "ops-diagnostics").trust, "reviewed");
    await rb.close();
  } finally {
    await d.stop();
  }
});

test("commands outside the rules are refused", async () => {
  const d = await daemon("terminal");
  try {
    const rb = new Rbridge({ app: "ops-check", env: d.env });
    const decisions = [];
    const res = await check({ dir: d.project, rb, log: (m) => decisions.push(m) });
    assert.match(res.text, /terminal refused/);
    assert.ok(decisions.some((m) => m.startsWith("denied")), decisions.join("\n"));
    await rb.close();
  } finally {
    await d.stop();
  }
});
