package ai_test

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bory/karvon-be/internal/campaign/ai"
)

// An over-long variant name is shortened to fit email_variants.name, but the cut is
// made at a byte offset: ref.Name[:MaxNameLen]. A name whose MaxNameLen'th byte sits
// in the middle of a multi-byte rune — any accent, dash or emoji a model reaches for
// — is sliced in half, and the string that comes out is not valid UTF-8.
//
// PostgreSQL refuses invalid UTF-8 outright ("invalid byte sequence for encoding
// UTF8"), so importing that generation fails on the insert rather than at the point
// the name was mangled. Truncation has to happen on rune boundaries.
//
// Note also that the column's CHECK counts CHARACTERS (length(name) BETWEEN 1 AND
// 120) while the Go side counts BYTES, so a non-ASCII name is cut shorter than the
// column actually requires.
func TestParseOutputTruncatesVariantNamesOnRuneBoundaries(t *testing.T) {
	// 119 ASCII bytes, then "é" (2 bytes): byte 120 falls inside the "é".
	longName := strings.Repeat("a", 119) + "é" + " trailing words"

	payload := map[string]any{
		"components": []map[string]any{
			{"type": "subject", "name": "Subject", "body": "Quick question", "tags": []string{}},
			{"type": "cta", "name": "CTA", "body": "Worth 15 minutes?", "tags": []string{}},
		},
		"variants": []map[string]any{
			{"name": longName, "subject": 0, "cta": 1},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	out, err := ai.ParseOutput(string(raw))
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	if len(out.Variants) != 1 {
		t.Fatalf("got %d variants, want 1", len(out.Variants))
	}

	name := out.Variants[0].Name
	if !utf8.ValidString(name) {
		t.Errorf("variant name is not valid UTF-8 after truncation: %q", name)
	}
	if n := utf8.RuneCountInString(name); n > ai.MaxNameLen {
		t.Errorf("variant name is %d characters, want at most %d", n, ai.MaxNameLen)
	}
}
