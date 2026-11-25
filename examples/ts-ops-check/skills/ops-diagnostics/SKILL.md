---
name: ops-diagnostics
description: Checks the health of the services listed in services.json and of the machine they run on, and recommends what to do.
license: Apache-2.0
---

# Operations health check

The working directory holds `services.json`: a list of services, each with a `name` and a health `url`.

Commands need approval, and only single commands are approved: run each one on its own, never joined with `&&`, `;` or `|`, and do not change directory first.

1. Run this skill's `scripts/health.sh` by its full path, with `services.json` from the working directory as its argument. It prints one line per service: `name status http_code seconds`.
2. Check the machine with read-only commands only: `uptime`, `df -h`, `free -m`. Do not install, restart, delete or change anything.
3. Decide an overall status: `ok` when every service answers with 2xx, `down` when none does, `degraded` otherwise.
4. For each problem, recommend one concrete next step an operator can take.
5. If the answer format is given as a JSON schema, follow it exactly.
