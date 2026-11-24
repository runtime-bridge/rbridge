package domain

import (
	"maps"
	"slices"
)

// CapabilityName identifies something a runtime may be able to do.
type CapabilityName string

const (
	CapStreaming          CapabilityName = "streaming"
	CapSessionsPersistent CapabilityName = "sessions.persistent"
	CapSessionsLoad       CapabilityName = "sessions.load"
	CapSessionsResume     CapabilityName = "sessions.resume"
	CapCancel             CapabilityName = "cancel"
	CapSkills             CapabilityName = "skills"
	CapMCPStdio           CapabilityName = "mcp.stdio"
	CapMCPHTTP            CapabilityName = "mcp.http"
	CapFilesystem         CapabilityName = "filesystem"
	CapShell              CapabilityName = "shell"
	CapStructuredOutput   CapabilityName = "structured_output"
	CapApprovals          CapabilityName = "approvals"
	CapPromptImage        CapabilityName = "prompt.image"
	CapPromptAudio        CapabilityName = "prompt.audio"
	CapUsage              CapabilityName = "usage"
	CapOffline            CapabilityName = "offline"
)

// KnownCapabilities lists every capability name rbridge understands, in display order.
var KnownCapabilities = []CapabilityName{
	CapStreaming, CapSessionsPersistent, CapSessionsLoad, CapSessionsResume, CapCancel,
	CapSkills, CapMCPStdio, CapMCPHTTP, CapFilesystem, CapShell, CapStructuredOutput,
	CapApprovals, CapPromptImage, CapPromptAudio, CapUsage, CapOffline,
}

// Support says how far a capability is supported.
type Support string

const (
	SupportYes     Support = "yes"
	SupportNo      Support = "no"
	SupportPartial Support = "partial"
	SupportUnknown Support = "unknown"
)

// CapabilitySource records where a capability value came from.
// Values from a higher-ranked source replace lower-ranked ones.
type CapabilitySource string

const (
	SourceDefault   CapabilitySource = "default"
	SourceManifest  CapabilitySource = "manifest"
	SourceProbe     CapabilitySource = "probe"
	SourceHandshake CapabilitySource = "handshake"
)

func (s CapabilitySource) rank() int {
	switch s {
	case SourceManifest:
		return 1
	case SourceProbe:
		return 2
	case SourceHandshake:
		return 3
	default:
		return 0
	}
}

// Capability describes one capability of one runtime.
type Capability struct {
	Supported Support           `json:"supported" yaml:"supported"`
	Mode      string            `json:"mode,omitempty" yaml:"mode,omitempty"`
	Source    CapabilitySource  `json:"source" yaml:"-"`
	Details   map[string]string `json:"details,omitempty" yaml:"details,omitempty"`
}

// CapabilitySet maps capability names to their description.
type CapabilitySet map[CapabilityName]Capability

// Get returns the capability, or an unknown value with a default source.
func (s CapabilitySet) Get(name CapabilityName) Capability {
	if c, ok := s[name]; ok {
		return c
	}
	return Capability{Supported: SupportUnknown, Source: SourceDefault}
}

// Merge returns a new set where each entry of other replaces the entry in s
// only when other's source ranks at least as high.
func (s CapabilitySet) Merge(other CapabilitySet) CapabilitySet {
	out := make(CapabilitySet, len(s)+len(other))
	maps.Copy(out, s)
	for k, v := range other {
		cur, ok := out[k]
		if !ok || v.Source.rank() >= cur.Source.rank() {
			out[k] = v
		}
	}
	return out
}

// Names returns the capability names in the set, known ones first in display order.
func (s CapabilitySet) Names() []CapabilityName {
	seen := map[CapabilityName]bool{}
	var out []CapabilityName
	for _, n := range KnownCapabilities {
		if _, ok := s[n]; ok {
			out = append(out, n)
			seen[n] = true
		}
	}
	var rest []CapabilityName
	for n := range s {
		if !seen[n] {
			rest = append(rest, n)
		}
	}
	slices.Sort(rest)
	return append(out, rest...)
}
