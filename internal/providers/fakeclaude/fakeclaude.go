// Package fakeclaude imitates Claude Code's headless stream-json protocol, as
// recorded in spike S3, for tests. Tests re-run their own binary with
// RBRIDGE_FAKE_CLAUDE set and call Main from TestMain.
package fakeclaude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	EnvEnable   = "RBRIDGE_FAKE_CLAUDE"
	EnvScenario = "RBRIDGE_FAKE_CLAUDE_SCENARIO"
	// EnvArgsFile, when set, receives the command line for inspection.
	EnvArgsFile = "RBRIDGE_FAKE_CLAUDE_ARGS"
)

type fake struct {
	mu        sync.Mutex
	out       *bufio.Writer
	scenario  string
	sessionID string
	pending   map[string]chan map[string]any
	interrupt chan struct{}
	inited    bool
}

func (f *fake) send(v map[string]any) {
	b, _ := json.Marshal(v)
	f.mu.Lock()
	f.out.Write(append(b, '\n'))
	f.out.Flush()
	f.mu.Unlock()
}

// Main runs until stdin closes.
func Main() {
	f := &fake{out: bufio.NewWriter(os.Stdout), scenario: os.Getenv(EnvScenario), pending: map[string]chan map[string]any{}}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println("9.9.9 (Claude Code)")
		return
	}
	if p := os.Getenv(EnvArgsFile); p != "" {
		_ = os.WriteFile(p, []byte(strings.Join(args, "\n")), 0o644)
	}
	for i, a := range args {
		if (a == "--session-id" || a == "--resume") && i+1 < len(args) {
			f.sessionID = args[i+1]
		}
	}

	turns := make(chan string, 4)
	go func() {
		for t := range turns {
			f.runTurn(t)
		}
	}()

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m["type"] {
		case "control_request":
			req, _ := m["request"].(map[string]any)
			id, _ := m["request_id"].(string)
			switch req["subtype"] {
			case "initialize":
				f.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{"commands": []any{}}}})
			case "interrupt":
				f.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{"still_queued": []any{}}}})
				if f.scenario != "hang" {
					f.mu.Lock()
					if f.interrupt != nil {
						close(f.interrupt)
						f.interrupt = nil
					}
					f.mu.Unlock()
				}
			}
		case "control_response":
			resp, _ := m["response"].(map[string]any)
			id, _ := resp["request_id"].(string)
			f.mu.Lock()
			ch := f.pending[id]
			f.mu.Unlock()
			if ch != nil {
				inner, _ := resp["response"].(map[string]any)
				ch <- inner
			}
		case "user":
			msg, _ := m["message"].(map[string]any)
			text, _ := msg["content"].(string)
			turns <- text
		}
	}
	close(turns)
}

func (f *fake) init() {
	if f.inited {
		return
	}
	f.inited = true
	cwd, _ := os.Getwd()
	var skills []string
	for i, a := range os.Args {
		if a == "--add-dir" {
			for _, d := range os.Args[i+1:] {
				if strings.HasPrefix(d, "--") {
					break
				}
				entries, _ := os.ReadDir(filepath.Join(d, ".claude", "skills"))
				for _, e := range entries {
					skills = append(skills, e.Name())
				}
			}
		}
	}
	f.send(map[string]any{"type": "system", "subtype": "init", "session_id": f.sessionID, "cwd": cwd, "model": "fake-model", "apiKeySource": "none", "skills": skills})
}

func (f *fake) assistant(blocks ...map[string]any) {
	f.send(map[string]any{"type": "assistant", "session_id": f.sessionID, "message": map[string]any{"role": "assistant", "content": blocks}})
}

func (f *fake) text(s string) { f.assistant(map[string]any{"type": "text", "text": s}) }

func (f *fake) result(text string, extra map[string]any) {
	r := map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text, "stop_reason": "end_turn", "session_id": f.sessionID,
		"total_cost_usd": 0.001, "usage": map[string]any{"input_tokens": 3, "output_tokens": 4}}
	for k, v := range extra {
		r[k] = v
	}
	f.send(r)
}

func (f *fake) ask(tool string, input map[string]any) map[string]any {
	id := "req-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	ch := make(chan map[string]any, 1)
	f.mu.Lock()
	f.pending[id] = ch
	f.mu.Unlock()
	f.send(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{"subtype": "can_use_tool", "tool_name": tool, "input": input, "tool_use_id": "toolu_1"}})
	return <-ch
}

func (f *fake) runTurn(text string) {
	cwd, _ := os.Getwd()
	switch f.scenario {
	case "", "echo":
		f.init()
		f.send(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "allowed"}})
		f.assistant(map[string]any{"type": "tool_use", "id": "toolu_r", "name": "Read", "input": map[string]any{"file_path": filepath.Join(cwd, "x")}})
		f.send(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_r", "content": "ok"}}}})
		f.text("echo: ")
		f.text(text)
		f.result("echo: "+text, nil)

	case "permission":
		f.init()
		target := filepath.Join(cwd, "notes.txt")
		f.assistant(map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Write", "input": map[string]any{"file_path": target, "content": "hi"}})
		resp := f.ask("Write", map[string]any{"file_path": target, "content": "notes from the fake runtime\n"})
		if resp["behavior"] == "allow" {
			_ = os.WriteFile(target, []byte("notes from the fake runtime\n"), 0o644)
			f.text("allowed")
			f.result("allowed", nil)
		} else {
			f.send(map[string]any{"type": "system", "subtype": "permission_denied", "tool_name": "Write", "decision_reason": fmt.Sprint(resp["message"])})
			f.text("denied")
			f.result("denied", nil)
		}

	case "slow":
		f.init()
		ch := make(chan struct{})
		f.mu.Lock()
		f.interrupt = ch
		f.mu.Unlock()
		for i := 0; ; i++ {
			select {
			case <-ch:
				f.send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "terminal_reason": "aborted_streaming", "session_id": f.sessionID})
				return
			case <-time.After(20 * time.Millisecond):
				f.text(strconv.Itoa(i) + " ")
			}
		}

	case "hang":
		select {}

	case "crash":
		f.init()
		f.text("about to crash")
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintln(os.Stderr, "fake claude: simulated crash")
		os.Exit(3)

	case "auth":
		f.init()
		f.result("Not logged in · Please run /login", map[string]any{"is_error": true})
		time.Sleep(20 * time.Millisecond)
		os.Exit(1)

	case "spawn":
		f.init()
		child := exec.Command("sleep", "60")
		if err := child.Start(); err == nil {
			_ = os.WriteFile(filepath.Join(cwd, "child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		}
		f.result("spawned", nil)

	case "schema":
		f.init()
		f.result(`{"ok":true}`, nil)
	}
}
