// Command fakeagent is the scriptable ACP agent from internal/providers/fakeacp
// as a standalone binary, for SDK tests and manual experiments. Choose the
// behaviour with RBRIDGE_FAKE_ACP_SCENARIO.
package main

import "github.com/runtime-bridge/rbridge/internal/providers/fakeacp"

func main() { fakeacp.Main() }
