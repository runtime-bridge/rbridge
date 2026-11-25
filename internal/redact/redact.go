// Package redact removes secrets from text before rbridge writes it to logs,
// event files, audit records or error details.
package redact

import (
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Mask replaces each secret.
const Mask = "[redacted]"

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`sk-(?:proj-|live_|test_)?[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{30,}`),
	regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// assignments catches name=value and "name": "value" pairs with secret-looking names.
var assignments = regexp.MustCompile(`(?i)((?:api[_-]?key|secret|token|password|passwd|auth|credential)[A-Za-z0-9_-]*["']?\s*[:=]\s*["']?)([^\s"',;]{6,})`)

// Redactor also masks exact values it was told about, such as the values of
// secret environment variables.
type Redactor struct {
	mu     sync.RWMutex
	values []string
}

// New returns a redactor that also masks the given values.
func New(values ...string) *Redactor {
	r := &Redactor{}
	r.Add(values...)
	return r
}

// Add registers more values. Short values are ignored, since masking them
// would destroy ordinary text.
func (r *Redactor) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		if len(v) >= 8 && !slices.Contains(r.values, v) {
			r.values = append(r.values, v)
		}
	}
	// Longest first, so a value containing another is masked whole.
	slices.SortFunc(r.values, func(a, b string) int { return len(b) - len(a) })
}

// FromEnviron registers the values of variables whose names look secret.
func (r *Redactor) FromEnviron(environ []string) *Redactor {
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if ok && SecretName(name) {
			r.Add(value)
		}
	}
	return r
}

// SecretName reports whether an environment variable name suggests a secret.
func SecretName(name string) bool {
	n := strings.ToUpper(name)
	for _, s := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "AUTH"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// String masks every secret in s.
func (r *Redactor) String(s string) string {
	if s == "" {
		return s
	}
	if r != nil {
		r.mu.RLock()
		for _, v := range r.values {
			s = strings.ReplaceAll(s, v, Mask)
		}
		r.mu.RUnlock()
	}
	for _, p := range patterns {
		s = p.ReplaceAllString(s, Mask)
	}
	return assignments.ReplaceAllString(s, "${1}"+Mask)
}

// Bytes is String for byte slices.
func (r *Redactor) Bytes(b []byte) []byte { return []byte(r.String(string(b))) }

// Map masks every value of a map in place and returns it.
func (r *Redactor) Map(m map[string]string) map[string]string {
	for k, v := range m {
		m[k] = r.String(v)
	}
	return m
}
