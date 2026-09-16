package sqlitedsn

import "testing"

func TestBuild(t *testing.T) {
	if got, want := Build("/tmp/a.db"), "/tmp/a.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"; got != want {
		t.Errorf("Build(no params) = %q, want %q", got, want)
	}
	if got, want := Build("/tmp/a.db?_pragma=foo(1)"), "/tmp/a.db?_pragma=foo(1)"; got != want {
		t.Errorf("Build(existing params) = %q, want unchanged %q", got, want)
	}
}
