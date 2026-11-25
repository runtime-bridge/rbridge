// Package audit keeps one record per session in monthly JSON Lines files.
// Records hold metadata only: no prompts, outputs or command lines.
package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
)

// Approval is one permission decision, without the command or path.
type Approval struct {
	Kind     string `json:"kind"`
	Tool     string `json:"tool,omitempty"`
	Decision string `json:"decision"` // allow or deny
	By       string `json:"by"`
	Reason   string `json:"reason,omitempty"`
}

// Record describes one session.
type Record struct {
	ID           string                 `json:"id"`
	Time         time.Time              `json:"time"`
	SessionID    string                 `json:"session_id"`
	Client       string                 `json:"client,omitempty"`
	Provider     string                 `json:"provider"`
	Workspace    string                 `json:"workspace"`
	Profile      string                 `json:"profile"`
	Skills       []domain.SkillDelivery `json:"skills,omitempty"`
	Approvals    []Approval             `json:"approvals,omitempty"`
	Fallback     *string                `json:"fallback"`
	Turns        int                    `json:"turns"`
	StopReason   domain.StopReason      `json:"stop_reason,omitempty"`
	ErrorCode    domain.ErrorCode       `json:"error_code,omitempty"`
	ResultDigest string                 `json:"result_digest,omitempty"`
	Unenforced   []string               `json:"unenforced_controls,omitempty"`
}

// Log writes and reads audit records.
type Log struct {
	Dir       string
	Retention time.Duration
	Now       func() time.Time
	mu        sync.Mutex
}

func (l *Log) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

func fileFor(t time.Time) string { return t.UTC().Format("2006-01") + ".jsonl" }

// Append writes a record and removes months older than the retention period.
func (l *Log) Append(r Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.Time.IsZero() {
		r.Time = l.now()
	}
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(l.Dir, fileFor(r.Time)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	l.prune()
	return nil
}

func (l *Log) prune() {
	if l.Retention <= 0 {
		return
	}
	cutoff := l.now().Add(-l.Retention)
	entries, _ := os.ReadDir(l.Dir)
	for _, e := range entries {
		month, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok {
			continue
		}
		start, err := time.Parse("2006-01", month)
		if err != nil {
			continue
		}
		// A file covers its whole month; drop it once the month ended before the cutoff.
		if start.AddDate(0, 1, 0).Before(cutoff) {
			os.Remove(filepath.Join(l.Dir, e.Name()))
		}
	}
}

// Read returns records at or after since, oldest first.
func (l *Log) Read(since time.Time) ([]Record, error) {
	entries, err := os.ReadDir(l.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	var out []Record
	for _, n := range names {
		f, err := os.Open(filepath.Join(l.Dir, n))
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			var r Record
			if json.Unmarshal(sc.Bytes(), &r) == nil && !r.Time.Before(since) {
				out = append(out, r)
			}
		}
		f.Close()
	}
	return out, nil
}
