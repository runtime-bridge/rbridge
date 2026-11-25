package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/runtime-bridge/rbridge/internal/config"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

var digest = "sha256:" + strings.Repeat("ab", 32)

func serve(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, "http://example.com/index.json", http.StatusFound)
			return
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch(t *testing.T) {
	index := `{"version":1,"skills":[
	 {"id":"security-audit","description":"Finds security problems","license":"MIT","git":{"url":"https://example.com/s.git","commit":"` + commit + `","subdir":"security-audit"},"digest":"` + digest + `"},
	 {"id":"floating","git":{"url":"https://example.com/s.git","commit":"main"},"digest":"` + digest + `"},
	 {"id":"plain-http","git":{"url":"http://example.com/s.git","commit":"` + commit + `"},"digest":"` + digest + `"},
	 {"id":"no-digest","git":{"url":"https://example.com/s.git","commit":"` + commit + `"}},
	 {"id":"escape","git":{"url":"https://example.com/s.git","commit":"` + commit + `","subdir":"../x"},"digest":"` + digest + `"},
	 {"id":"security-audit","git":{"url":"https://example.com/s.git","commit":"` + commit + `"},"digest":"` + digest + `"}]}`
	srv := serve(t, index, 200)
	entries, dropped, err := Client{}.Fetch(context.Background(), config.CatalogConfig{Name: "team", URL: srv.URL + "/index.json"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Catalog != "team" || entries[0].Git.Subdir != "security-audit" {
		t.Fatalf("entries %+v", entries)
	}
	if len(dropped) != 5 {
		t.Fatalf("dropped %v", dropped)
	}
	if !Match(entries[0], "SECURITY problems") || Match(entries[0], "security pdf") || !Match(entries[0], "") {
		t.Error("match")
	}

	for name, c := range map[string]config.CatalogConfig{
		"not found":     {Name: "x", URL: serve(t, "", 404).URL},
		"bad json":      {Name: "x", URL: serve(t, "{", 200).URL},
		"bad version":   {Name: "x", URL: serve(t, `{"version":2,"skills":[]}`, 200).URL},
		"too large":     {Name: "x", URL: serve(t, `{"version":1,"skills":[],"pad":"`+strings.Repeat("x", MaxIndexBytes)+`"}`, 200).URL},
		"redirect away": {Name: "x", URL: srv.URL + "/moved"},
		"remote http":   {Name: "x", URL: "http://example.com/index.json"},
	} {
		if _, _, err := (Client{}).Fetch(context.Background(), c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
