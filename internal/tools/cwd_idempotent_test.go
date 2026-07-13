package tools

import "testing"

// A path that ALREADY carries the cwd must not be joined onto the cwd again.
//
// The model is handed paths constantly and feeds them straight back: `cd` reports
// its new dir ("openhands"), git_clone reports where it landed, list_dir echoes
// entry paths. All of them now speak ONE namespace — node-relative — so
// read_file("openhands/README.md") after `cd openhands` is unambiguous and must
// WORK. Doubling the cwd into openhands/openhands/README.md is never what anyone
// means: a live explorer node made 34 REPEATED calls out of 69 flailing through
// variants of exactly that.
func TestJoinCwd_DoesNotDoubleTheCwd(t *testing.T) {
	const cwd = "openhands"

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "cwd-relative path is joined (the normal case)",
			path: "README.md",
			want: "openhands/README.md",
		},
		{
			name: "a path that already carries the cwd is taken as-is",
			path: "openhands/README.md",
			want: "openhands/README.md",
		},
		{
			name: "the cwd itself is taken as-is",
			path: "openhands",
			want: "openhands",
		},
		{
			name: "a sibling that merely shares a prefix is still joined",
			path: "openhands-v2/x.go",
			want: "openhands/openhands-v2/x.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := joinCwd(cwd, tt.path); got != tt.want {
				t.Fatalf("joinCwd(%q, %q) = %q, want %q", cwd, tt.path, got, tt.want)
			}
		})
	}
}

// With no cwd set, paths pass through unchanged.
func TestJoinCwd_NoCwd(t *testing.T) {
	if got := joinCwd("", "a/b.go"); got != "a/b.go" {
		t.Fatalf("joinCwd(\"\", %q) = %q", "a/b.go", got)
	}
}
