package verify

import "strings"

// Typo detection thresholds. A short domain has to match almost exactly before we
// call it a misspelling; a long one can differ by two edits, because there is more
// room to slip and far less chance of a coincidental near-match.
const (
	typoMaxDistanceShort = 1
	typoMaxDistanceLong  = 2
	typoLongDomainLength = 10
)

// TypoSuggestion returns the domain from the reference list that the given domain is
// probably a misspelling of, or an empty string.
//
// A domain that is itself on the reference list, or that is a known mail provider, is
// never flagged. Everything else is compared by Damerau-Levenshtein distance. The
// check is advisory on purpose: it caps the score so the address is never paid for,
// and the correction is offered to the operator rather than applied automatically.
func (l *Lists) TypoSuggestion(domain string) string {
	domain = normalizeDomain(domain)
	if domain == "" || l.IsTopDomain(domain) || l.IsFreeProvider(domain) {
		return ""
	}

	limit := typoMaxDistanceShort
	if len(domain) >= typoLongDomainLength {
		limit = typoMaxDistanceLong
	}

	best, bestDistance := "", limit+1
	for _, candidate := range l.topDomains {
		// A cheap length gate keeps the comparison off most of the list.
		if abs(len(candidate)-len(domain)) > limit {
			continue
		}
		distance := damerauLevenshtein(domain, candidate)
		if distance > 0 && distance <= limit && distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	return best
}

// SuggestAddress rebuilds a full address against a suggested domain.
func SuggestAddress(local, domain string) string {
	if domain == "" {
		return ""
	}
	return local + "@" + domain
}

// damerauLevenshtein is the optimal string alignment distance: insertions,
// deletions, substitutions and transpositions of adjacent characters.
func damerauLevenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	rows, cols := len(ar)+1, len(br)+1

	previous := make([]int, cols)
	current := make([]int, cols)
	beforePrevious := make([]int, cols)

	for j := 0; j < cols; j++ {
		previous[j] = j
	}

	for i := 1; i < rows; i++ {
		current[0] = i
		for j := 1; j < cols; j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			current[j] = min3(
				current[j-1]+1,     // insertion
				previous[j]+1,      // deletion
				previous[j-1]+cost, // substitution
			)
			if i > 1 && j > 1 && ar[i-1] == br[j-2] && ar[i-2] == br[j-1] {
				if swapped := beforePrevious[j-2] + 1; swapped < current[j] {
					current[j] = swapped
				}
			}
		}
		beforePrevious, previous, current = previous, current, beforePrevious
	}
	return previous[cols-1]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// RegistrableDomain trims a leading "www." so a website-derived domain and a
// mail domain compare equal.
func RegistrableDomain(domain string) string {
	return strings.TrimPrefix(normalizeDomain(domain), "www.")
}
