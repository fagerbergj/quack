// Package sqlitedsn builds a sqlite DSN with WAL/busy-timeout/FK pragmas -
// a leaf package so internal/store and internal/pluginreg can share it
// without an import cycle (same rationale as internal/pgdial).
package sqlitedsn

import "strings"

// Build enables WAL + busy timeout + FK enforcement (SQLite defaults
// foreign_keys OFF per connection). Existing query params are left
// untouched - a caller with its own params has already made that choice.
func Build(url string) string {
	if strings.Contains(url, "?") {
		return url
	}
	return url + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
}
