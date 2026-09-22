package business

import "testing"

// An address with more than one "@" is not an address. NormalizeEmail splits at the
// FIRST "@", so everything after it lands in `domain`; the guard that was meant to
// catch a stray "@" tests `local`, which by construction can never contain one.
//
// The consequence is not cosmetic: a value like "a@b@c.com" passes AcceptEmail, is
// stored in business_emails, exported to CSV, imported as a contact and pushed to
// Instantly, which rejects it. The verification pipeline splits on the LAST "@"
// instead (see verify.SplitAddress), so the two halves of the system do not even
// agree on what the local part is.
func TestNormalizeEmailRejectsMoreThanOneAt(t *testing.T) {
	inputs := []string{
		"a@b@c.com",
		"info@@ironworksgym.com",
		"a@b@c@d.com",
		"amy@ironworksgym.com@evil.example",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			if got, ok := NormalizeEmail(in); ok {
				t.Errorf("NormalizeEmail(%q) = %q, true; an address may contain exactly one @", in, got)
			}
			if got, ok := AcceptEmail(in); ok {
				t.Errorf("AcceptEmail(%q) = %q, true; an address may contain exactly one @", in, got)
			}
		})
	}
}

// The single-@ addresses around the rule must keep working.
func TestNormalizeEmailStillAcceptsOrdinaryAddresses(t *testing.T) {
	for _, in := range []string{"amy@ironworksgym.com", "info+leads@ironworksgym.com"} {
		if _, ok := NormalizeEmail(in); !ok {
			t.Errorf("NormalizeEmail(%q) rejected an ordinary address", in)
		}
	}
}
