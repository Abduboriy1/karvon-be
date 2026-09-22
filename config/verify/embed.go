// Package verifylists embeds the blocklists Pass 1 checks against, so the container
// works with no extra files mounted. Every list can be replaced at runtime by
// pointing KARVON_VERIFY_LISTS_DIR at a directory holding files of the same names.
package verifylists

import "embed"

// FS holds the default lists shipped with the binary.
//
//go:embed *.txt
var FS embed.FS

// Names of the lists, used both for the embedded files and for the override directory.
const (
	FileDisposable    = "disposable_domains.txt"
	FileRoleHard      = "role_hard.txt"
	FileRoleSoft      = "role_soft.txt"
	FileFreeProviders = "free_providers.txt"
	FileTopDomains    = "top_domains.txt"
)

// All lists the loader reads.
var All = []string{FileDisposable, FileRoleHard, FileRoleSoft, FileFreeProviders, FileTopDomains}
