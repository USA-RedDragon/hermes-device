package memory

import (
	"slices"
	"strings"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// condensedLevels are the levels of conclusions Honcho's dreams write.
func condensedLevels() []string {
	return []string{hub.LevelDeductive, hub.LevelInductive, hub.LevelContradiction}
}

// level is a conclusion's level. Honcho's default is explicit.
func level(l string) string {
	if l == "" {
		return hub.LevelExplicit
	}
	return l
}

func condensed(l string) bool { return level(l) != hub.LevelExplicit }

// levelRank orders levels from single message to most condensed.
func levelRank(l string) int {
	switch level(l) {
	case hub.LevelDeductive:
		return 1
	case hub.LevelInductive:
		return 2
	case hub.LevelContradiction:
		return 3
	}
	return 0
}

// factKey is the text two copies of one fact share: case and runs of
// white space do not count. Any other difference, such as punctuation or
// wording, makes two facts, so a delete never removes text the user did not
// see.
func factKey(content string) string {
	return strings.ToLower(strings.Join(strings.Fields(content), " "))
}

// fact is one distinct fact and its stored copies.
type fact struct {
	hub.Conclusion
	hermes, self bool
}

func (f *fact) in(view string) bool {
	switch view {
	case hub.ViewHermes:
		return f.hermes
	case hub.ViewSelf:
		return f.self
	}
	return true
}

func (f *fact) add(h *honcho, c conclusionJSON) {
	switch c.ObserverID {
	case h.ai:
		f.hermes = true
	case h.user:
		f.self = true
	}
	if levelRank(c.Level) > levelRank(f.Level) {
		f.Level = level(c.Level)
	}
	if c.CreatedAt.Before(f.CreatedAt) {
		f.ID, f.CreatedAt = c.ID, c.CreatedAt
		if c.SessionID != nil {
			f.SessionID = *c.SessionID
		}
	}
	if f.SessionID == "" && c.SessionID != nil {
		f.SessionID = *c.SessionID
	}
	switch {
	case f.hermes && f.self:
		f.View = hub.ViewBoth
	case f.hermes:
		f.View = hub.ViewHermes
	default:
		f.View = hub.ViewSelf
	}
}

func (h *honcho) newFact(c conclusionJSON) *fact {
	f := &fact{Conclusion: h.conclusion(c)}
	f.IDs = []string{c.ID}
	f.add(h, c)
	return f
}

// facts pairs the copies of each fact among conclusions read oldest first.
// It keeps only conclusions about the user, and orders the facts by their
// oldest copy. byKey finds a fact by its factKey.
func (h *honcho) facts(cs []conclusionJSON) (list []*fact, byKey map[string]*fact) {
	byKey = map[string]*fact{}
	seen := map[string]bool{}
	for _, c := range cs {
		if !h.about(c) || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		k := factKey(c.Content)
		if f := byKey[k]; f != nil {
			f.IDs = append(f.IDs, c.ID)
			f.add(h, c)
			continue
		}
		f := h.newFact(c)
		byKey[k] = f
		list = append(list, f)
	}
	slices.SortStableFunc(list, func(a, b *fact) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return list, byKey
}
