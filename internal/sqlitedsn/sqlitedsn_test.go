package sqlitedsn

import "testing"

func TestBuild(t *testing.T) {
	const prod = "/tmp/a.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if got := build("/tmp/a.db", false); got != prod {
		t.Errorf("build(no params, prod) = %q, want %q", got, prod)
	}
	if got, want := Build("/tmp/a.db"), prod+"&_pragma=synchronous(OFF)"; got != want {
		t.Errorf("Build(no params) in a test binary = %q, want %q", got, want)
	}
	if got, want := Build("/tmp/a.db?_pragma=foo(1)"), "/tmp/a.db?_pragma=foo(1)"; got != want {
		t.Errorf("Build(existing params) = %q, want unchanged %q", got, want)
	}
}
