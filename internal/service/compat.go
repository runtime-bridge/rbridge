package service

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
)

const compatCollection = "skillcompat"

// SkillCompat is what rbridge has measured about one skill on one runtime.
// It is built from real sessions, never from what a publisher claims.
type SkillCompat struct {
	SkillID        string    `json:"skill_id"`
	Provider       string    `json:"provider"`
	Status         string    `json:"status"` // works, delivered, failed, delivery_failed
	Digest         string    `json:"digest,omitempty"`
	RuntimeVersion string    `json:"runtime_version,omitempty"`
	Mode           string    `json:"mode,omitempty"`
	Sessions       int       `json:"sessions"`
	Used           int       `json:"used"`
	TurnsCompleted int       `json:"turns_completed"`
	TurnsFailed    int       `json:"turns_failed"`
	LastError      string    `json:"last_error,omitempty"`
	CheckedAt      time.Time `json:"checked_at"`
}

// skillWatch notices a runtime opening a delivered skill: a tool event that
// touches one of the skill's folders, or a skill tool naming it.
type skillWatch struct {
	mu      sync.Mutex
	needles map[string][]string
	used    map[string]bool
}

func newSkillWatch() *skillWatch {
	return &skillWatch{needles: map[string][]string{}, used: map[string]bool{}}
}

func (w *skillWatch) add(id string, dirs ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, d := range dirs {
		if d != "" {
			w.needles[id] = append(w.needles[id], d)
		}
	}
	w.needles[id] = append(w.needles[id], `"skill":"`+id+`"`, `"name":"`+id+`"`)
}

func (w *skillWatch) observe(e domain.Event) {
	if w == nil || (e.Type != domain.EventToolStarted && e.Type != domain.EventToolUpdated) {
		return
	}
	data := string(e.Data)
	data = strings.ReplaceAll(data, `\\`, `\`)
	data = strings.ReplaceAll(data, `": "`, `":"`)
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, needles := range w.needles {
		if w.used[id] {
			continue
		}
		for _, n := range needles {
			if strings.Contains(data, n) {
				w.used[id] = true
				break
			}
		}
	}
}

func (w *skillWatch) wasUsed(id string) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.used[id]
}

func compatKey(skill, provider string) string { return skill + "." + provider }

// recordCompat folds one session into the stored results.
func (s *Service) recordCompat(skill domain.SkillDelivery, provider, version string, used bool, completed, failed int, lastErr domain.ErrorCode) {
	ctx := context.Background()
	var c SkillCompat
	ok, _ := s.store.Get(ctx, compatCollection, compatKey(skill.SkillID, provider), &c)
	if !ok || c.Digest != skill.Digest || c.RuntimeVersion != version {
		// A new skill version or runtime version starts a fresh measurement.
		c = SkillCompat{SkillID: skill.SkillID, Provider: provider}
	}
	c.Digest, c.RuntimeVersion, c.Mode = skill.Digest, version, skill.Mode
	c.Sessions++
	c.TurnsCompleted += completed
	c.TurnsFailed += failed
	if used {
		c.Used++
	}
	if lastErr != "" {
		c.LastError = string(lastErr)
	}
	switch {
	case c.Used > 0 && c.TurnsCompleted > 0:
		c.Status = "works"
	case c.TurnsCompleted > 0:
		c.Status = "delivered"
	case c.TurnsFailed > 0:
		c.Status = "failed"
	default:
		c.Status = "delivered"
	}
	c.CheckedAt = s.now().UTC()
	_ = s.store.Put(ctx, compatCollection, compatKey(skill.SkillID, provider), c)
}

// recordDeliveryFailure notes that a skill could not be delivered to a runtime.
func (s *Service) recordDeliveryFailure(ctx context.Context, ids []string, provider, version string, err error) {
	reg := s.Skills()
	for _, id := range ids {
		rec, ok, _ := reg.Get(ctx, id)
		if !ok {
			continue
		}
		c := SkillCompat{
			SkillID: id, Provider: provider, Status: "delivery_failed", Digest: rec.Digest, RuntimeVersion: version,
			LastError: string(domain.CodeOf(err)), CheckedAt: s.now().UTC(),
		}
		var prev SkillCompat
		if ok, _ := s.store.Get(ctx, compatCollection, compatKey(id, provider), &prev); ok && prev.Digest == rec.Digest && prev.RuntimeVersion == version {
			c.Sessions = prev.Sessions
		}
		_ = s.store.Put(ctx, compatCollection, compatKey(id, provider), c)
	}
}

// SkillCompatibility returns the measured results for a skill, one per
// runtime. Results for an older version of the skill are left out.
func (s *Service) SkillCompatibility(ctx context.Context, id string) ([]SkillCompat, error) {
	rec, err := s.Skills().Require(ctx, id)
	if err != nil {
		return nil, err
	}
	keys, err := s.store.List(ctx, compatCollection)
	if err != nil {
		return nil, err
	}
	out := []SkillCompat{}
	for _, k := range keys {
		if !strings.HasPrefix(k, id+".") {
			continue
		}
		var c SkillCompat
		if ok, _ := s.store.Get(ctx, compatCollection, k, &c); ok && c.SkillID == id && c.Digest == rec.Digest {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out, nil
}
