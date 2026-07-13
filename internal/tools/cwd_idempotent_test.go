package tools

import "testing"

// A path that ALREADY carries the cwd must not be joined onto the cwd again.
//
// The live failure: with the node-scoped cwd, `cd openhands` reports its new dir
// workspace-relative — "explorer-openhands/openhands". The model then does the
// natural thing and feeds that path straight back:
//
//	read_file("explorer-openhands/openhands/README.md")
//
// which used to resolve to
//
//	<chat>/explorer-openhands/openhands/explorer-openhands/openhands/README.md
//
// and fail. The model then flails through variants. One live explorer node made
// 34 REPEATED calls out of 69 doing exactly this; the node looked like it was
// spinning. Doubling the cwd is never what anyone means.
func TestJoinCwd_DoesNotDoubleTheCwd(t *testing.T) {
	const cwd = "explorer-openhands/openhands"

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "cwd-relative path is joined (the normal case)",
			path: "README.md",
			want: "explorer-openhands/openhands/README.md",
		},
		{
			name: "a path that already carries the cwd is taken as-is",
			path: "explorer-openhands/openhands/README.md",
			want: "explorer-openhands/openhands/README.md",
		},
		{
			name: "the cwd itself is taken as-is",
			path: "explorer-openhands/openhands",
			want: "explorer-openhands/openhands",
		},
		{
			name: "a leading slash still escapes to the scope root",
			path: "/other-node/repo/x.go",
			want: "other-node/repo/x.go",
		},
		{
			name: "a sibling that merely shares a prefix is still joined",
			path: "explorer-openhands-v2/x.go",
			want: "explorer-openhands/openhands/explorer-openhands-v2/x.go",
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
