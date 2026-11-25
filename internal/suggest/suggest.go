// Package suggest finds the option a mistyped word most likely meant.
package suggest

import "strings"

// Closest returns the option nearest to input, or "" when none is near
// enough to be a likely typo. A prefix in either direction counts as near
// ("claude" for "claude-code", "skill" for "skills").
func Closest(input string, options []string) string {
	in := strings.ToLower(input)
	best, bestDist := "", 0
	for _, o := range options {
		lo := strings.ToLower(o)
		if lo == in {
			return o
		}
		d := distance(in, lo)
		if len(in) >= 3 && (strings.HasPrefix(lo, in) || strings.HasPrefix(in, lo)) {
			d = 1
		}
		if best == "" || d < bestDist {
			best, bestDist = o, d
		}
	}
	limit := 1
	if len(in) >= 4 {
		limit = max(2, len(in)/3)
	}
	if best == "" || bestDist > limit {
		return ""
	}
	return best
}

// DidYouMean is "; did you mean X?" for the closest option, or "".
func DidYouMean(input string, options []string) string {
	if c := Closest(input, options); c != "" {
		return "; did you mean " + c + "?"
	}
	return ""
}

func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
