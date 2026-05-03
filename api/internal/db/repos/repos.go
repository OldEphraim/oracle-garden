package repos

import (
	"fmt"
	"strings"
)

// substitutePlaceholder rewrites the rightmost "?" in s into "$N" for the
// pgx-friendly numbered-parameter form. Used by the dynamic List builders
// to keep the construction local instead of threading $-counts through
// every helper.
func substitutePlaceholder(s string, n int) string {
	idx := strings.LastIndex(s, "?")
	if idx < 0 {
		return s
	}
	return s[:idx] + fmt.Sprintf("$%d", n) + s[idx+1:]
}

// joinComma joins parts with ", ". Reimplemented here only because the
// stdlib's strings.Join needs a slice; this is a tiny readability helper
// for the SET-clause builder.
func joinComma(parts []string) string {
	return strings.Join(parts, ", ")
}
