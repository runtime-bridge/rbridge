# Operations health check from Node.js

An operations tool can package its know-how as a skill and let the agent runtime the user already has run the check, within rules the tool sets. `check.js` gives rbridge an `ops-diagnostics` skill (a procedure and a health script), lets whichever runtime rbridge picks check the services in `services.json` and the machine, and prints a verdict. It is plain JavaScript; editors get types from the SDK's declarations.

```sh
npm install                       # installs @rbridge/sdk from GitHub
rbridge daemon start
node check.js                     # checks sample/services.json
node check.js ~/ops --provider claude-code --model haiku --sandbox files
```

A run with Claude Code against `sample/` (one service up, one down) reports `degraded`, with each check, the machine's load, disk and memory, and a next step for the service that is down. The exit code is 1 when everything is down.

What it shows:

- **Commands under the application's rules.** The task runs under the `controlled` profile, so every command needs approval. `approve()` allows reading, the skill's own `health.sh` and `uptime`, `df -h` and `free -m`, each on its own, and denies everything else, including combined command lines. Every decision is printed.
- **Scripts follow trust.** The skill is marked reviewed, so its script is delivered; an unreviewed skill would arrive without it.
- **An optional sandbox.** With `--sandbox files` on Linux, even a command that slipped through could not write outside the project.

`check.test.js` tests the approval rules and runs the check against a daemon whose runtime is rbridge's fake agent, including a session where the agent tries a command the rules refuse: `npm test` after `make build fakeagent`.
