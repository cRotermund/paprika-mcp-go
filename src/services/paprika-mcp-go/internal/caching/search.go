package caching

import (
	"context"
	"slices"
	"strings"
)

// Query selects recipes. Every word in Text must match somewhere. Words match
// as prefixes of whole words, case-insensitively, in the name, category names,
// source, and ingredients. Directions and notes are not searchable.
type Query struct {
	Text string
	// CategoryUID keeps only recipes in that category.
	CategoryUID string
	// Favorite, when set, keeps only recipes whose favorite flag equals it.
	Favorite *bool
	// MinRating keeps only recipes rated at least this high.
	MinRating int
	// IncludeTrashed also returns recipes in the trash.
	IncludeTrashed bool
}

// Ranking weights: a word found in the name outranks one found in a category
// or the source, which outranks one found only in the ingredients.
const (
	weightName       = 10000
	weightCategory   = 100
	weightIngredient = 1
)

// maxQueryWords bounds the words used from Text.
const maxQueryWords = 10

type hit struct {
	entry *IndexEntry
	score int
	key   string // lowercase name, for ordering
}

// Search returns matching recipes, best first: by score, then by name. With no
// text it returns every recipe passing the filters in name order. The entries
// are shared and must not be modified. While the index is loading it returns a
// [*LoadingError].
func (c *Cache) Search(ctx context.Context, q Query) ([]*IndexEntry, error) {
	if p := c.Progress(); !p.Ready {
		return nil, &LoadingError{Indexed: p.Indexed, Total: p.Total}
	}
	if err := c.ensureRecipesFresh(ctx); err != nil {
		c.log.Warn("could not check for recipe changes; searching held index", "error", err)
	}

	qwords := words(q.Text)
	if len(qwords) > maxQueryWords {
		qwords = qwords[:maxQueryWords]
	}

	var categoryWords map[string][]string
	if len(qwords) > 0 {
		cats, err := c.Categories(ctx)
		if err != nil {
			return nil, err
		}
		categoryWords = make(map[string][]string, len(cats))
		for _, cat := range cats {
			categoryWords[cat.Value.UID] = words(cat.Value.Name)
		}
	}

	c.mu.RLock()
	all := make([]*IndexEntry, 0, len(c.index))
	for _, e := range c.index {
		all = append(all, e)
	}
	c.mu.RUnlock()

	hits := make([]hit, 0, len(all))
	for _, e := range all {
		if e.InTrash && !q.IncludeTrashed {
			continue
		}
		if e.Rating < q.MinRating {
			continue
		}
		if q.Favorite != nil && e.Favorite != *q.Favorite {
			continue
		}
		if q.CategoryUID != "" && !slices.Contains(e.Categories, q.CategoryUID) {
			continue
		}
		score, ok := scoreEntry(e, qwords, categoryWords)
		if !ok {
			continue
		}
		hits = append(hits, hit{entry: e, score: score, key: strings.ToLower(e.Name)})
	}

	slices.SortFunc(hits, func(a, b hit) int {
		if a.score != b.score {
			return b.score - a.score
		}
		if c := strings.Compare(a.key, b.key); c != 0 {
			return c
		}
		return strings.Compare(a.entry.UID, b.entry.UID)
	})

	out := make([]*IndexEntry, len(hits))
	for i, h := range hits {
		out[i] = h.entry
	}
	return out, nil
}

// scoreEntry returns the entry's score, and false if some query word matches nowhere.
func scoreEntry(e *IndexEntry, qwords []string, categoryWords map[string][]string) (int, bool) {
	if len(qwords) == 0 {
		return 0, true
	}
	nameWords := words(e.Name)
	sourceWords := words(e.Source)
	score := 0
	for _, qw := range qwords {
		switch {
		case anyPrefix(nameWords, qw):
			score += weightName
		case anyPrefix(sourceWords, qw) || anyCategory(e.Categories, categoryWords, qw):
			score += weightCategory
		case anyPrefix(e.Tokens, qw):
			score += weightIngredient
		default:
			return 0, false
		}
	}
	return score, true
}

func anyCategory(uids []string, categoryWords map[string][]string, qw string) bool {
	for _, uid := range uids {
		if anyPrefix(categoryWords[uid], qw) {
			return true
		}
	}
	return false
}
