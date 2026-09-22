package verify

import (
	"net/mail"
	"strings"
)

// Address length limits from RFC 5321 section 4.5.3.1.
const (
	MaxLocalLength   = 64
	MaxAddressLength = 254
	maxDomainLabel   = 63
)

// NormalizeAddress lower-cases and trims an address so the same mailbox always
// produces the same row. It matches what the scraper stores, which is what makes the
// citext unique index on email_verifications meaningful.
func NormalizeAddress(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// SplitAddress separates an address at its last "@".
func SplitAddress(email string) (local, domain string, ok bool) {
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return "", "", false
	}
	return email[:at], email[at+1:], true
}

// validSyntax reports whether the address parses as a bare RFC 5322 addr-spec.
//
// A maintained parser is used rather than a regular expression, and the result is
// required to be exactly the input: "Gym <a@b.com>" parses, but it is a name plus an
// address, not an address, and accepting it would store the wrong string.
func validSyntax(email string) bool {
	if email == "" || len(email) > MaxAddressLength {
		return false
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil {
		return false
	}
	return parsed.Name == "" && parsed.Address == email
}

// checkStructure applies the length and shape rules that a bare RFC parser allows
// but real mail systems reject.
func checkStructure(local, domain string) (bool, string) {
	switch {
	case local == "":
		return false, "the local part is empty"
	case len(local) > MaxLocalLength:
		return false, "the local part is longer than 64 characters"
	case len(local)+len(domain)+1 > MaxAddressLength:
		return false, "the address is longer than 254 characters"
	case strings.HasPrefix(local, `"`) || strings.HasSuffix(local, `"`):
		return false, "the local part is quoted"
	case strings.HasPrefix(local, "."):
		return false, "the local part starts with a dot"
	case strings.HasSuffix(local, "."):
		return false, "the local part ends with a dot"
	case strings.Contains(local, ".."):
		return false, "the local part contains consecutive dots"
	}

	if !strings.Contains(domain, ".") {
		return false, "the domain has no dot"
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false, "the domain starts or ends with a dot"
	}
	if strings.Contains(domain, "..") {
		return false, "the domain contains consecutive dots"
	}

	labels := strings.Split(domain, ".")
	for _, label := range labels {
		switch {
		case label == "":
			return false, "the domain has an empty label"
		case len(label) > maxDomainLabel:
			return false, "a domain label is longer than 63 characters"
		case strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-"):
			return false, "a domain label starts or ends with a hyphen"
		}
		for _, r := range label {
			if !isDomainRune(r) {
				return false, "the domain contains an unexpected character"
			}
		}
	}

	tld := labels[len(labels)-1]
	if len(tld) < 2 || !isAlphaASCII(tld) {
		return false, "the top-level domain is not alphabetic"
	}
	return true, ""
}

func isDomainRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
}

func isAlphaASCII(s string) bool {
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return s != ""
}

// baseLocal strips plus-addressing so "info+leads@" is recognised as "info@".
func baseLocal(local string) string {
	if plus := strings.Index(local, "+"); plus > 0 {
		return local[:plus]
	}
	return local
}
