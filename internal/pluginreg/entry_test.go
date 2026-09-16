package pluginreg

import "testing"

func TestParseEntry(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Entry
	}{
		{"bare repo, untracked", "github:fagerbergj/dotagents", Entry{
			Raw: "github:fagerbergj/dotagents", Source: SourceGitHub, Owner: "fagerbergj", Repo: "dotagents",
		}},
		{"pinned tag", "github:fagerbergj/ponytail@v1.4", Entry{
			Raw: "github:fagerbergj/ponytail@v1.4", Source: SourceGitHub, Owner: "fagerbergj", Repo: "ponytail", Ref: "v1.4",
		}},
		{"subdirectory path, untracked", "github:fagerbergj/quack-extensions#usage", Entry{
			Raw: "github:fagerbergj/quack-extensions#usage", Source: SourceGitHub, Owner: "fagerbergj", Repo: "quack-extensions", Path: "usage",
		}},
		{"pinned ref and path", "github:fagerbergj/quack-extensions@main#usage/skills", Entry{
			Raw: "github:fagerbergj/quack-extensions@main#usage/skills", Source: SourceGitHub,
			Owner: "fagerbergj", Repo: "quack-extensions", Ref: "main", Path: "usage/skills",
		}},
		{"pinned to a sha", "github:fagerbergj/dotagents@c886ce1a8474939dc42f7c194f8c57242223ea19", Entry{
			Raw: "github:fagerbergj/dotagents@c886ce1a8474939dc42f7c194f8c57242223ea19", Source: SourceGitHub,
			Owner: "fagerbergj", Repo: "dotagents", Ref: "c886ce1a8474939dc42f7c194f8c57242223ea19",
		}},
		{"trailing .git is stripped from the repo name", "github:fagerbergj/dotagents.git", Entry{
			Raw: "github:fagerbergj/dotagents.git", Source: SourceGitHub, Owner: "fagerbergj", Repo: "dotagents",
		}},
		{"redundant #. path means the repo root", "github:fagerbergj/dotagents#.", Entry{
			Raw: "github:fagerbergj/dotagents#.", Source: SourceGitHub, Owner: "fagerbergj", Repo: "dotagents",
		}},
		{"local path", ".agents/vendor/dotagents", Entry{
			Raw: ".agents/vendor/dotagents", Source: SourceLocal, Root: ".agents/vendor/dotagents",
		}},
		{"local manifest-only plugin", ".agents/plugins/usage", Entry{
			Raw: ".agents/plugins/usage", Source: SourceLocal, Root: ".agents/plugins/usage",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseEntry(tc.in)
			if err != nil {
				t.Fatalf("ParseEntry(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseEntry(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseEntryMalformed(t *testing.T) {
	cases := []string{
		"",
		"github:",
		"github:owner",
		"github:owner/",
		"github:/repo",
		"github:owner/repo/extra",
		"github:owner/repo@",
		"github:owner/repo#",
		"github:owner/repo@ref#",
		"github:owner//repo",
		"github:owner/repo@-x",              // ref must not start with "-" (flag injection)
		"github:owner /repo",                // whitespace in owner
		"github:owner/repo @v1",             // whitespace in ref
		"github:owner/repo#usage skills",    // whitespace in path
		"github:owner/..",                   // repo ".." (name traversal, issue #5)
		"github:../repo",                    // owner ".." likewise
		"github:owner/repo#..",              // path escapes the plugin root
		"github:owner/repo#../../etc",       // path escapes the plugin root
		"github:owner/repo#/etc/passwd",     // absolute path
		"github:owner/repo#skills/../../..", // escapes after cleaning
		".",                                 // degenerate local root
		"..",                                // degenerate local root
		"/",                                 // degenerate local root
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := ParseEntry(in); err == nil {
				t.Fatalf("ParseEntry(%q) succeeded, want an error naming the syntax", in)
			}
		})
	}
}

func TestEntryName(t *testing.T) {
	e, err := ParseEntry("github:fagerbergj/dotagents@v2#skills")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name() != "dotagents" {
		t.Fatalf("Name() = %q, want dotagents", e.Name())
	}
	local, err := ParseEntry(".agents/vendor/dotagents")
	if err != nil {
		t.Fatal(err)
	}
	if local.Name() != "dotagents" {
		t.Fatalf("local Name() = %q, want the path's last element", local.Name())
	}
}

func TestValidName(t *testing.T) {
	bad := []string{"", ".", "..", "a/b", `a\b`}
	for _, n := range bad {
		if err := validName(n); err == nil {
			t.Fatalf("validName(%q) accepted an unsafe registry name", n)
		}
	}
	if err := validName("widgets"); err != nil {
		t.Fatalf("validName(\"widgets\") = %v, want nil", err)
	}
}
