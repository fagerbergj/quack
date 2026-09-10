package tools

import "testing"

// A path that ALREADY carries the cwd must not be joined onto the cwd again:
// every tool speaks ONE node-relative namespace, so read_file("openhands/README.md")
// after `cd openhands` is unambiguous and must WORK (a live explorer node flailed through 34 of 69 calls doubling it to openhands/openhands/...).
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
