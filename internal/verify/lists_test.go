package verify

import (
	"os"
	"path/filepath"
	"testing"

	verifylists "github.com/bory/karvon-be/config/verify"
)

func TestEmbeddedListsLoad(t *testing.T) {
	lists, err := LoadLists("")
	if err != nil {
		t.Fatalf("LoadLists returned %v", err)
	}
	if !lists.IsDisposable("mailinator.com") {
		t.Error("mailinator.com is not on the disposable list")
	}
	if !lists.IsDisposable("mail.guerrillamail.com") {
		t.Error("a subdomain of a disposable apex was not matched")
	}
	if lists.IsDisposable("ironworksgym.com") {
		t.Error("a normal domain was treated as disposable")
	}
	if !lists.IsRoleHard("noreply") || !lists.IsRoleHard("postmaster") {
		t.Error("the automated mailbox list is missing entries")
	}
	if !lists.IsRoleSoft("info") || !lists.IsRoleSoft("sales") {
		t.Error("the shared mailbox list is missing entries")
	}
	if lists.IsRoleHard("info") {
		t.Error("info@ is on the hard list; it belongs on the soft one")
	}
	if !lists.IsFreeProvider("gmail.com") || lists.IsFreeProvider("ironworksgym.com") {
		t.Error("the free provider list is wrong")
	}
	if !lists.IsTopDomain("gmail.com") {
		t.Error("gmail.com is not on the typo reference list")
	}
	if len(lists.TopDomains()) < 50 {
		t.Errorf("the typo reference list has only %d entries", len(lists.TopDomains()))
	}
}

// A list can be replaced without rebuilding the binary, which is how the disposable
// list is kept current.
func TestListsCanBeOverriddenFromDisk(t *testing.T) {
	dir := t.TempDir()
	override := "# operator list\nexample-temp.test\nMAILINATOR.com\n"
	if err := os.WriteFile(filepath.Join(dir, verifylists.FileDisposable), []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}

	lists, err := LoadLists(dir)
	if err != nil {
		t.Fatalf("LoadLists returned %v", err)
	}
	if !lists.IsDisposable("example-temp.test") {
		t.Error("the override file was not used")
	}
	if !lists.IsDisposable("mailinator.com") {
		t.Error("entries are not lower-cased when read")
	}
	if lists.IsDisposable("yopmail.com") {
		t.Error("the override should replace the embedded list, not extend it")
	}
	// Lists with no override still come from the binary.
	if !lists.IsRoleHard("noreply") {
		t.Error("a list without an override was not loaded from the embedded copy")
	}
}

func TestTypoSuggestion(t *testing.T) {
	lists, err := LoadLists("")
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]string{
		"gmial.com":         "gmail.com",
		"gmai.com":          "gmail.com",
		"gmail.co":          "gmail.com",
		"hotmial.com":       "hotmail.com",
		"yahooo.com":        "yahoo.com",
		"outlok.com":        "outlook.com",
		"gmail.com":         "",
		"ironworksgym.com":  "",
		"austinbarbell.com": "",
	}
	for domain, want := range tests {
		if got := lists.TypoSuggestion(domain); got != want {
			t.Errorf("TypoSuggestion(%q) = %q, want %q", domain, got, want)
		}
	}
}

func TestDamerauLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"gmail.com", "gmail.com", 0},
		{"gmial.com", "gmail.com", 1},
		{"gmai.com", "gmail.com", 1},
		{"gmail.co", "gmail.com", 1},
		{"ggmail.com", "gmail.com", 1},
		{"yahoo.com", "gmail.com", 5},
		{"", "abc", 3},
		{"abc", "", 3},
	}
	for _, tt := range tests {
		if got := damerauLevenshtein(tt.a, tt.b); got != tt.want {
			t.Errorf("damerauLevenshtein(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
