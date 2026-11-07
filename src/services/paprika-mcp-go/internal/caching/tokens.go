package caching

import (
	"slices"
	"strings"
	"unicode"
)

const (
	minIngredientTokenLen = 3
	maxIngredientTokens   = 100
)

// stopwords are common ingredient words that would only bloat the index.
var stopwords = map[string]struct{}{
	"and":         {},
	"the":         {},
	"for":         {},
	"with":        {},
	"into":        {},
	"cup":         {},
	"cups":        {},
	"tablespoon":  {},
	"tablespoons": {},
	"tbsp":        {},
	"teaspoon":    {},
	"teaspoons":   {},
	"tsp":         {},
	"ounce":       {},
	"ounces":      {},
	"pound":       {},
	"pounds":      {},
	"gram":        {},
	"grams":       {},
	"chopped":     {},
	"diced":       {},
	"sliced":      {},
	"minced":      {},
	"large":       {},
	"small":       {},
	"medium":      {},
	"fresh":       {},
	"freshly":     {},
	"finely":      {},
	"optional":    {},
	"taste":       {},
	"about":       {},
	"cut":         {},
	"pieces":      {},
	"piece":       {},
	"divided":     {},
}

func isWordBreak(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// words splits text into lowercase words.
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), isWordBreak)
}

func isNumeric(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

// ingredientTokens returns the sorted, deduplicated search words of an
// ingredient list. Numbers, very short words, and common unit words are dropped.
func ingredientTokens(text string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, w := range words(text) {
		if len([]rune(w)) < minIngredientTokenLen || isNumeric(w) {
			continue
		}
		if _, skip := stopwords[w]; skip {
			continue
		}
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	slices.Sort(out)
	if len(out) > maxIngredientTokens {
		out = out[:maxIngredientTokens]
	}
	return out
}

// anyPrefix reports whether any word starts with prefix.
func anyPrefix(ws []string, prefix string) bool {
	for _, w := range ws {
		if strings.HasPrefix(w, prefix) {
			return true
		}
	}
	return false
}
