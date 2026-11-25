// Package catalog reads skill catalogs: JSON indexes served over HTTP that
// point to skills in Git repositories, each pinned to a commit and to the
// digest rbridge computes for the skill's files.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/runtime-bridge/rbridge/internal/config"
)

// MaxIndexBytes bounds a catalog index.
const MaxIndexBytes = 4 << 20

// Entry is one skill in a catalog.
type Entry struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	License     string `json:"license,omitempty"`
	Git         struct {
		URL    string `json:"url"`
		Commit string `json:"commit"`
		Subdir string `json:"subdir,omitempty"`
	} `json:"git"`
	// Digest is rbridge's digest of the skill folder, "sha256:<hex>".
	Digest string `json:"digest"`
	// Catalog is the configured name of the catalog the entry came from.
	Catalog string `json:"catalog"`
}

// Index is a catalog document.
type Index struct {
	Version int     `json:"version"`
	Skills  []Entry `json:"skills"`
}

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	idPattern     = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// Client fetches catalogs.
type Client struct {
	HTTP *http.Client
}

func (c Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		// A redirect must keep to the same rules as the configured URL.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return config.CheckCatalogURL(req.URL.String())
		},
	}
}

// Fetch downloads and validates one catalog. Entries that are not pinned to
// a full commit and a digest are dropped and reported.
func (c Client) Fetch(ctx context.Context, cat config.CatalogConfig) ([]Entry, []string, error) {
	if err := config.CheckCatalogURL(cat.URL); err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cat.URL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog %s: %w", cat.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("catalog %s: HTTP %d", cat.Name, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxIndexBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("catalog %s: %w", cat.Name, err)
	}
	if len(body) > MaxIndexBytes {
		return nil, nil, fmt.Errorf("catalog %s: index is larger than %d bytes", cat.Name, MaxIndexBytes)
	}
	var idx Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, nil, fmt.Errorf("catalog %s: not a valid index: %w", cat.Name, err)
	}
	if idx.Version != 1 {
		return nil, nil, fmt.Errorf("catalog %s: unsupported index version %d", cat.Name, idx.Version)
	}
	var out []Entry
	var dropped []string
	seen := map[string]bool{}
	for _, e := range idx.Skills {
		why := ""
		switch {
		case !idPattern.MatchString(e.ID) || len(e.ID) > 64:
			why = "invalid id"
		case seen[e.ID]:
			why = "listed twice"
		case !strings.HasPrefix(e.Git.URL, "https://"):
			why = "git url must use https"
		case !commitPattern.MatchString(e.Git.Commit):
			why = "not pinned to a full commit"
		case !digestPattern.MatchString(e.Digest):
			why = "no sha256 digest"
		case strings.Contains(e.Git.Subdir, "..") || strings.HasPrefix(e.Git.Subdir, "/"):
			why = "subdir leaves the repository"
		}
		if why != "" {
			dropped = append(dropped, fmt.Sprintf("%s: %s", e.ID, why))
			continue
		}
		seen[e.ID] = true
		e.Catalog = cat.Name
		out = append(out, e)
	}
	return out, dropped, nil
}

// Match reports whether an entry matches a search query (all words, any
// case, in the id or description).
func Match(e Entry, query string) bool {
	text := strings.ToLower(e.ID + " " + e.Description)
	return !slices.ContainsFunc(strings.Fields(strings.ToLower(query)), func(w string) bool {
		return !strings.Contains(text, w)
	})
}
