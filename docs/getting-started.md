# Getting started

This walk-through takes about ten minutes. You need one agent runtime installed and signed in: Claude Code (`claude`) or OpenCode (`opencode`) with a model provider set up. rbridge never signs in for you.

## 1. Install

```sh
curl -fsSL https://raw.githubusercontent.com/runtime-bridge/rbridge/main/scripts/install.sh | sh
```

The script checks the archive against the release's checksums (and its cosign signature if `cosign` is installed) before writing `~/.local/bin/rbridge`. With Go 1.25 or later you can instead run `go install github.com/runtime-bridge/rbridge/cmd/rbridge@latest`.

## 2. Check what rbridge finds

```sh
rbridge doctor
rbridge providers
```

`doctor` prints one line per check with a fix for anything that is missing. `providers show claude-code` (or `opencode`) lists what that runtime can do and where it looks for skills.

## 3. Run a task

```sh
mkdir -p ~/rbridge-demo && cd ~/rbridge-demo
printf 'def add(a, b):\n    return a + b\n' > util.py
rbridge run --profile observe "Explain what util.py does in one sentence"
```

The answer streams to the terminal; status lines go to stderr. `observe` lets the runtime read but not change anything. Under the default `standard` profile, edits inside the workspace are allowed and commands are asked about.

## 4. Add a skill

A skill is a folder with a `SKILL.md`. Make a small one:

```sh
mkdir -p ~/rbridge-skills/one-line-summary
cat > ~/rbridge-skills/one-line-summary/SKILL.md <<'SKILL'
---
name: one-line-summary
description: Summarise a file in exactly one line that starts with "Summary:". Use when asked for a one-line summary.
license: Apache-2.0
---

Read the file you are asked about and answer with exactly one line that starts with "Summary:".
SKILL
rbridge skills add ~/rbridge-skills/one-line-summary
rbridge skills show one-line-summary
rbridge skills trust one-line-summary reviewed
rbridge run --skill one-line-summary --profile observe "Use the one-line-summary skill on util.py"
```

New skills start as `unreviewed` and are refused until you trust them. rbridge hands the skill to the runtime for this run only.

## 5. Use it from your own program

Go, embedded in your process:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/runtime-bridge/rbridge/pkg/bridge"
)

func main() {
	ctx := context.Background()
	b, err := bridge.New(ctx, bridge.Options{})
	if err != nil {
		log.Fatal(err)
	}
	ws, _ := os.Getwd()
	res, err := b.Run(ctx, bridge.Task{
		Workspace: ws,
		Prompt:    bridge.Text("Use the one-line-summary skill on util.py"),
		Skills:    []string{"one-line-summary"},
		Profile:   "observe",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Output)
}
```

Other languages talk to the daemon:

```sh
rbridge daemon start
```

```js
import { connect } from "@rbridge/sdk";
const rb = await connect({ client: "demo" });
console.log((await rb.run({ workspace: process.cwd(), prompt: "Explain util.py", profile: "observe" })).output);
rb.close();
```

```python
import os, rbridge
with rbridge.connect(client="demo") as rb:
    print(rb.run({"workspace": os.getcwd(), "prompt": "Explain util.py", "profile": "observe"})["output"])
```

Stop the daemon with `rbridge daemon stop`.

## 6. See what happened

```sh
rbridge sessions
rbridge audit
```

`audit` shows one line per session: the profile, how many requests were allowed or denied, and which controls rbridge could not enforce.

## Next

- [Configuration](configuration.md)
- [Threat model](threat-model.md)
- HTTP and IPC protocols: `specs/http-api.yaml`, `specs/ipc-protocol.md`
