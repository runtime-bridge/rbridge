package redact

import (
	"strings"
	"testing"
)

func TestPatterns(t *testing.T) {
	secrets := []string{
		"sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWx",
		"sk-proj-abcdefghijklmnopqrstuvwxyz012345",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"github_pat_11ABCDEFG0123456789abcdefghijklmnop",
		"xoxb-1234567890-abcdefghij",
		"AKIAIOSFODNN7EXAMPLE",
		"AIzaSyA1234567890abcdefghijklmnopqrstuv",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefghijk123",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----",
	}
	r := New()
	for _, s := range secrets {
		out := r.String("value: " + s + " end")
		if strings.Contains(out, s) || !strings.Contains(out, Mask) || !strings.HasSuffix(out, " end") {
			t.Errorf("not redacted: %q", out)
		}
	}
	for in, keep := range map[string]string{
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz": "Authorization: ",
		`{"api_key": "hunter2hunter2"}`:                    `{"api_key": "`,
		"PASSWORD=correct-horse-battery":                   "PASSWORD=",
		"export GITHUB_TOKEN=abc123def456":                 "export GITHUB_TOKEN=",
	} {
		out := r.String(in)
		if !strings.HasPrefix(out, keep) || !strings.Contains(out, Mask) {
			t.Errorf("%q -> %q", in, out)
		}
	}
	ordinary := "build failed: undefined: tokenize (main.go:12); see the authors file"
	if got := r.String(ordinary); got != ordinary {
		t.Errorf("ordinary text changed: %q", got)
	}
}

func TestKnownValues(t *testing.T) {
	r := New().FromEnviron([]string{"MY_SERVICE_TOKEN=plainvalue123", "HOME=/home/u", "SHORT_KEY=abc", "DB_PASSWORD=p@ss-w0rd-long"})
	out := r.String("connecting with plainvalue123 as p@ss-w0rd-long from /home/u abc")
	if strings.Contains(out, "plainvalue123") || strings.Contains(out, "p@ss-w0rd-long") {
		t.Fatalf("known values leaked: %q", out)
	}
	if !strings.Contains(out, "/home/u") || !strings.HasSuffix(out, " abc") {
		t.Fatalf("non-secret values masked: %q", out)
	}
	var nilR *Redactor
	if nilR.String("sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWx") == "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWx" {
		t.Fatal("nil redactor must still apply patterns")
	}
}
