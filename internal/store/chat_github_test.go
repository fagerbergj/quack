package store

import (
	"encoding/json"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
)

func originJSON(t *testing.T, o extsdk.ChatOrigin) string {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestChatGitHub(t *testing.T) {
	gh := originJSON(t, extsdk.ChatOrigin{
		Extension: "github", Href: "https://github.com/acme/app/pull/7", State: extsdk.SubjectMerged,
		Labels: map[string][]extsdk.LabelValue{"repo": {{Value: "acme/app"}}},
	})
	cases := []struct {
		name              string
		chat              Chat
		repo, href, state string
	}{
		{"origin only", Chat{Origin: gh}, "acme/app", "https://github.com/acme/app/pull/7", "merged"},
		{"legacy columns only", Chat{GithubRepo: "o/r", GithubURL: "u", GithubState: "open"}, "o/r", "u", "open"},
		{"pre-State origin falls back per field", Chat{GithubState: "closed",
			Origin: originJSON(t, extsdk.ChatOrigin{Extension: "github", Href: "h"})}, "", "h", "closed"},
		{"other extension's origin is ignored", Chat{Origin: originJSON(t, extsdk.ChatOrigin{
			Extension: "remarkable", Href: "https://rm/doc", State: extsdk.SubjectOpen})}, "", "", ""},
		{"no origin", Chat{}, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, href, state := tc.chat.GitHub()
			if repo != tc.repo || href != tc.href || state != tc.state {
				t.Fatalf("GitHub() = (%q, %q, %q), want (%q, %q, %q)", repo, href, state, tc.repo, tc.href, tc.state)
			}
		})
	}
}
