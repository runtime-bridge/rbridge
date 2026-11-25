// Operations health check by an agent runtime installed on this machine.
//
// The program ships an ops-diagnostics skill with a health script and hands the
// check to whichever runtime rbridge picks. Commands need approval: this
// program allows the skill's script and a few read-only commands, and denies
// the rest. Start the daemon first: rbridge daemon start.

import path from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";
import { connect } from "@rbridge/sdk";

const here = path.dirname(fileURLToPath(import.meta.url));

export const schema = {
  type: "object",
  required: ["status", "checks"],
  properties: {
    status: { type: "string", enum: ["ok", "degraded", "down"] },
    checks: {
      type: "array",
      items: {
        type: "object",
        required: ["name", "status"],
        properties: { name: { type: "string" }, status: { type: "string" }, detail: { type: "string" } },
      },
    },
    actions: { type: "array", items: { type: "string" } },
  },
};

const readOnly = [/^uptime$/, /^df -h$/, /^free -m$/];

/**
 * Allows reading, the skill's health script and a few read-only commands, each
 * on its own; denies everything else, including combined command lines.
 */
export function approve(request, log = () => {}) {
  if (request.kind === "read" || request.kind === "search") {
    log(`allowed ${request.kind}: ${request.title}`);
    return true;
  }
  if (request.kind !== "execute") {
    log(`denied ${request.kind}: ${request.title}`);
    return false;
  }
  let command = "";
  try {
    const input = typeof request.input === "string" ? JSON.parse(request.input) : request.input;
    command = String(input?.command ?? "").trim();
  } catch {
    command = "";
  }
  command ||= request.title.replace(/^run /, "");
  const runsScript = /^(?:(?:sh|bash) )?(?:\S*\/)?scripts\/health\.sh(?: \S*services\.json)?$/.test(command);
  const allowed = runsScript || readOnly.some((r) => r.test(command));
  log(`${allowed ? "allowed" : "denied"}: ${command}`);
  return allowed;
}

/** Structured output when the runtime supports it, otherwise JSON in the text. */
export function verdict(result) {
  let data = result.structured;
  if (!data) {
    const m = /\{[\s\S]*\}/.exec(result.output ?? "");
    try {
      data = m ? JSON.parse(m[0]) : undefined;
    } catch {
      data = undefined;
    }
  }
  return data && typeof data.status === "string" && Array.isArray(data.checks) ? data : undefined;
}

export async function check({ dir, provider, model, sandbox, client, log = () => {} }) {
  const own = !client;
  const rb = client ?? (await connect({ client: "ops-check" }));
  try {
    await rb.addSkill(path.join(here, "skills", "ops-diagnostics"));
    // Reviewed: the script is delivered, and every command still needs approval.
    await rb.setSkillTrust("ops-diagnostics", "reviewed");
    const task = {
      workspace: path.resolve(dir),
      prompt: "Check the health of the services in services.json and of this machine.",
      skills: ["ops-diagnostics"],
      profile: "controlled",
      output_schema: schema,
    };
    if (provider) task.provider = provider;
    if (sandbox) task.sandbox = sandbox;
    return await rb.run(task, { model, approver: (req) => approve(req, log) });
  } finally {
    if (own) rb.close();
  }
}

async function main() {
  const { values, positionals } = parseArgs({
    allowPositionals: true,
    options: { provider: { type: "string" }, model: { type: "string" }, sandbox: { type: "string" }, quiet: { type: "boolean" } },
  });
  const dir = positionals[0] ?? path.join(here, "sample");
  const log = values.quiet ? () => {} : (m) => console.error(`[approval] ${m}`);
  const result = await check({ dir, ...values, log });
  const v = verdict(result);
  if (!v) {
    console.log(result.output);
    return 0;
  }
  console.log(`status: ${v.status}  (checked by ${result.provider})`);
  for (const c of v.checks) console.log(`  ${c.status.padEnd(8)} ${c.name}${c.detail ? ": " + c.detail : ""}`);
  for (const a of v.actions ?? []) console.log(`  next: ${a}`);
  return v.status === "down" ? 1 : 0;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().then(
    (code) => process.exit(code),
    (err) => {
      console.error(`ops-check failed: ${err.code ?? ""} ${err.message}`);
      process.exit(2);
    },
  );
}
