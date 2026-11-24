package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rbridge/rbridge/internal/audit"
	"github.com/rbridge/rbridge/internal/domain"
)

func cmdAudit(ctx context.Context, args []string, env Env) error {
	fs := newFlags("audit", env)
	since := fs.String("since", "168h", "how far back: a duration such as 24h, or a date such as 2026-11-01")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	from, err := parseSince(*since, time.Now())
	if err != nil {
		return err
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	recs, err := svc.Audit().Read(from)
	if err != nil {
		return err
	}
	if *asJSON {
		if recs == nil {
			recs = []audit.Record{}
		}
		return writeJSON(env.Stdout, recs)
	}
	if len(recs) == 0 {
		fmt.Fprintln(env.Stdout, "no audit records in that period")
		return nil
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tSESSION\tPROVIDER\tPROFILE\tTURNS\tSTOP\tAPPROVALS\tNOT ENFORCED")
	for _, r := range recs {
		allowed, denied := 0, 0
		for _, a := range r.Approvals {
			if a.Decision == "allow" {
				allowed++
			} else {
				denied++
			}
		}
		stop := string(r.StopReason)
		if r.ErrorCode != "" {
			stop = string(r.ErrorCode)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%d allowed, %d denied\t%s\n", r.Time.Local().Format("2006-01-02 15:04"), r.SessionID, r.Provider, r.Profile, r.Turns, dash(stop), allowed, denied, dash(strings.Join(r.Unenforced, ", ")))
	}
	return tw.Flush()
}

func parseSince(s string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, domain.Errorf(domain.ErrInvalidInput, "--since must be a duration like 24h or a date like 2026-11-01")
}
