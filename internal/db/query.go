package db

import (
	"sort"
	"strconv"
)

// argSet accumulates positional query arguments so filter clauses can be assembled
// without ever interpolating user input into SQL.
type argSet struct {
	args []any
}

// add appends v and returns its placeholder ("$1", "$2", ...).
func (a *argSet) add(v any) string {
	a.args = append(a.args, v)
	return "$" + strconv.Itoa(len(a.args))
}

func (a *argSet) values() []any { return a.args }

// sortSpec is a whitelisted ORDER BY fragment. User input is only ever used to look
// one of these up by key, never to build SQL.
type sortSpec struct {
	expr string
	desc bool
}

func (s sortSpec) orderBy(tiebreak string) string {
	dir := " ASC"
	if s.desc {
		dir = " DESC"
	}
	return " ORDER BY " + s.expr + dir + ", " + tiebreak + dir
}

// lookupSort resolves a "field:direction" string against a whitelist.
func lookupSort(specs map[string]sortSpec, requested string, fallback string) sortSpec {
	if spec, ok := specs[requested]; ok {
		return spec
	}
	return specs[fallback]
}

// sortKeys returns the whitelisted sort values for an endpoint, sorted for stable
// error messages.
func sortKeys(specs map[string]sortSpec) []string {
	out := make([]string, 0, len(specs))
	for key := range specs {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
