package verify

import (
	"math"
	"strings"
)

// Thresholds for the "looks like a person chose this" heuristic. They are tuned to
// pass ordinary mailboxes (first.last, jsmith, bookings2024) and fail machine-made
// ones (a7f3c9d21b04, hash-like tokens, keyboard mashing).
const (
	maxDigitRatio     = 0.5
	randomLocalLength = 12
	maxEntropyPerChar = 3.5
	minVowelRatio     = 0.2
	keyboardRunLength = 5
)

// keyboardRows are the sequences a keyboard mash walks along.
var keyboardRows = []string{
	"qwertyuiop",
	"asdfghjkl",
	"zxcvbnm",
	"1234567890",
}

// checkHumanLocal reports whether the local part looks like something a person would
// choose. The detail explains the verdict, because this is the check most likely to
// surprise someone reading the breakdown.
func checkHumanLocal(local string) (bool, string) {
	local = baseLocal(local)
	if local == "" {
		return false, "the local part is empty"
	}

	if ratio := digitRatio(local); ratio > maxDigitRatio {
		return false, "more than half the characters are digits"
	}
	if run := keyboardRun(local); run != "" {
		return false, "contains the keyboard sequence " + run
	}

	// Long unbroken strings are where random tokens live. A mailbox with a
	// separator ("first.last", "jane-doe") is treated as deliberate.
	if len(local) >= randomLocalLength && !strings.ContainsAny(local, "._-") {
		if entropy(local) > maxEntropyPerChar {
			return false, "looks randomly generated"
		}
		if vowelRatio(local) < minVowelRatio {
			return false, "has almost no vowels for its length"
		}
	}
	return true, ""
}

func digitRatio(s string) float64 {
	digits := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return float64(digits) / float64(len(s))
}

func vowelRatio(s string) float64 {
	vowels := 0
	for _, r := range s {
		if strings.ContainsRune("aeiouy", r) {
			vowels++
		}
	}
	return float64(vowels) / float64(len(s))
}

// entropy is Shannon entropy in bits per character. Real words repeat characters and
// land well under the threshold; hex and base32 tokens sit above it.
func entropy(s string) float64 {
	counts := make(map[rune]int, len(s))
	for _, r := range s {
		counts[r]++
	}
	total := float64(len([]rune(s)))
	var bits float64
	for _, count := range counts {
		p := float64(count) / total
		bits -= p * math.Log2(p)
	}
	return bits
}

// keyboardRun returns the first keyboard sequence of at least keyboardRunLength
// characters found in s, forwards or backwards, or an empty string.
func keyboardRun(s string) string {
	if len(s) < keyboardRunLength {
		return ""
	}
	for start := 0; start+keyboardRunLength <= len(s); start++ {
		window := s[start : start+keyboardRunLength]
		for _, row := range keyboardRows {
			if strings.Contains(row, window) || strings.Contains(reverseString(row), window) {
				return window
			}
		}
	}
	return ""
}

func reverseString(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}
