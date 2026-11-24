package domain

import "time"

// TrustState is how far rbridge may use a skill.
type TrustState string

const (
	TrustUnreviewed TrustState = "unreviewed"
	TrustReviewed   TrustState = "reviewed"
	TrustTrusted    TrustState = "trusted"
	TrustRestricted TrustState = "restricted"
	TrustBlocked    TrustState = "blocked"
)

// InstructionsUsable reports whether a skill's instructions may be delivered.
// Unreviewed skills additionally depend on policy, so they report false here.
func (t TrustState) InstructionsUsable() bool {
	return t == TrustReviewed || t == TrustTrusted || t == TrustRestricted
}

// ScriptsAllowed reports whether a skill's scripts may be delivered at all.
// Reviewed skills still need an approval for each run.
func (t TrustState) ScriptsAllowed() bool {
	return t == TrustReviewed || t == TrustTrusted
}

// SkillSource records where a skill came from.
type SkillSource struct {
	Type     string `json:"type"` // "local" or "git"
	Path     string `json:"path,omitempty"`
	URL      string `json:"url,omitempty"`
	Revision string `json:"revision,omitempty"`
	Subdir   string `json:"subdir,omitempty"`
}

// SkillRecord is the registry entry for one installed skill version.
type SkillRecord struct {
	ID          string      `json:"id"`
	Version     string      `json:"version,omitempty"`
	Description string      `json:"description"`
	Source      SkillSource `json:"source"`
	Digest      string      `json:"digest"`
	License     string      `json:"license,omitempty"`
	Trust       TrustState  `json:"trust"`
	HasScripts  bool        `json:"has_scripts"`
	InstalledAt time.Time   `json:"installed_at"`
}
