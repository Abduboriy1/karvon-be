package verify

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	verifylists "github.com/bory/karvon-be/config/verify"
)

// Lists holds the blocklists Pass 1 consults. They are loaded once at boot; changing
// a list needs a restart, which is documented in the README.
type Lists struct {
	disposable    map[string]struct{}
	roleHard      map[string]struct{}
	roleSoft      map[string]struct{}
	freeProviders map[string]struct{}
	topDomains    []string
	topDomainSet  map[string]struct{}
}

// LoadLists reads the embedded defaults, then replaces any list for which a file of
// the same name exists in dir. An empty dir uses the embedded lists only.
func LoadLists(dir string) (*Lists, error) {
	sets := make(map[string]map[string]struct{}, len(verifylists.All))
	order := make(map[string][]string, len(verifylists.All))

	for _, name := range verifylists.All {
		lines, err := readList(dir, name)
		if err != nil {
			return nil, err
		}
		set := make(map[string]struct{}, len(lines))
		for _, line := range lines {
			set[line] = struct{}{}
		}
		sets[name] = set
		order[name] = lines
	}

	return &Lists{
		disposable:    sets[verifylists.FileDisposable],
		roleHard:      sets[verifylists.FileRoleHard],
		roleSoft:      sets[verifylists.FileRoleSoft],
		freeProviders: sets[verifylists.FileFreeProviders],
		topDomains:    order[verifylists.FileTopDomains],
		topDomainSet:  sets[verifylists.FileTopDomains],
	}, nil
}

// readList prefers an override file on disk and falls back to the embedded copy.
func readList(dir, name string) ([]string, error) {
	if dir != "" {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path) //nolint:gosec // operator-supplied configuration path
		switch {
		case err == nil:
			return parseList(string(data)), nil
		case !os.IsNotExist(err):
			return nil, fmt.Errorf("verify: read %s: %w", path, err)
		}
	}

	data, err := fs.ReadFile(verifylists.FS, name)
	if err != nil {
		return nil, fmt.Errorf("verify: read embedded %s: %w", name, err)
	}
	return parseList(string(data)), nil
}

// parseList drops comments, blanks and casing so a hand-edited file is forgiving.
func parseList(raw string) []string {
	var out []string
	seen := make(map[string]struct{})

	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.ToLower(line)
		if _, dup := seen[line]; dup {
			continue
		}
		seen[line] = struct{}{}
		out = append(out, line)
	}
	return out
}

// IsDisposable reports whether a domain, or any apex above it, is a throwaway host.
func (l *Lists) IsDisposable(domain string) bool { return matchDomain(l.disposable, domain) }

// IsFreeProvider reports whether a domain is consumer webmail.
func (l *Lists) IsFreeProvider(domain string) bool { return matchDomain(l.freeProviders, domain) }

// IsRoleHard reports whether a local part is an automated or abuse mailbox.
func (l *Lists) IsRoleHard(local string) bool {
	_, ok := l.roleHard[local]
	return ok
}

// IsRoleSoft reports whether a local part is a shared business mailbox.
func (l *Lists) IsRoleSoft(local string) bool {
	_, ok := l.roleSoft[local]
	return ok
}

// IsTopDomain reports whether a domain is itself on the reference list, in which case
// it cannot be a typo of another entry.
func (l *Lists) IsTopDomain(domain string) bool {
	_, ok := l.topDomainSet[domain]
	return ok
}

// TopDomains is the reference list typo detection measures against.
func (l *Lists) TopDomains() []string { return l.topDomains }

// matchDomain tests the domain and every parent apex, so "mail.mailinator.com"
// matches an entry for "mailinator.com".
func matchDomain(set map[string]struct{}, domain string) bool {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == "" {
		return false
	}
	for candidate := domain; candidate != ""; {
		if _, ok := set[candidate]; ok {
			return true
		}
		_, rest, found := strings.Cut(candidate, ".")
		if !found {
			return false
		}
		candidate = rest
	}
	return false
}
