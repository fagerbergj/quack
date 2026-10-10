// Package sqlitedsn builds a sqlite DSN with WAL/busy-timeout/FK pragmas, a leaf package so store and
// pluginreg can share it without an import cycle.
package sqlitedsn

import "strings"

// Build enables WAL, busy timeout and FK enforcement (SQLite defaults foreign_keys off per connection).
// A URL that already has query params is left untouched.
func Build(url string) string {
	if strings.Contains(url, "?") {
		return url
	}
	return url + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
}
