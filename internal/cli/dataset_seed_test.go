package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
)

func TestLineMutations(t *testing.T) {
	type mut struct{ Op, Mutated string }
	for text, want := range map[string][]mut{
		"\tif a == b {":                       {{"flip_eq", "\tif a != b {"}},
		"\tif err != nil {":                   {{"flip_eq", "\tif err == nil {"}},
		"\tfor i := 0; i < n; i++ {":          {{"boundary", "\tfor i := 0; i <= n; i++ {"}},
		"\treturn x >= y":                     {{"boundary", "\treturn x > y"}},
		"\tif !ok {":                          {{"drop_not", "\tif ok {"}},
		"\t\treturn nil, err":                 {{"return_nil", "\t\treturn nil, nil"}},
		"\treturn err // keep":                {{"return_nil", "\treturn nil // keep"}},
		"\treturn fmt.Errorf(\"x: %w\", err)": nil,
		"\ts := \"a == b\" // x != y":         nil,
		"\tch <- v; x := y << 2":              nil,
		"\tok := a && b":                      {{"and_or", "\tok := a || b"}},
		"\treturn a || f(\"x && y\")":         {{"and_or", "\treturn a && f(\"x && y\")"}},
		"\tx := s[i+1:]":                      {{"off_by_one", "\tx := s[i:]"}},
		"\tn := f(x) - 1 // last":             {{"off_by_one", "\tn := f(x) // last"}},
		"\tfor i := 0; i < len(xs); i++ {":    {{"boundary", "\tfor i := 0; i <= len(xs); i++ {"}, {"off_by_one", "\tfor i := 0; i < len(xs)-1; i++ {"}},
		"\ty := b[:len(b)]":                   {{"off_by_one", "\ty := b[:len(b)-1]"}},
		"\tm := x - 1*k; n := -1; f(-1)":      nil,
		"\th := len(b) / 2; c := cap(len(b))": nil,
		"\tz := len(b) == 0 || b[len(b)-k:]":  {{"flip_eq", "\tz := len(b) != 0 || b[len(b)-k:]"}, {"and_or", "\tz := len(b) == 0 && b[len(b)-k:]"}},
		"\tq := r.len(x) < 2":                 {{"boundary", "\tq := r.len(x) <= 2"}},
		"\tdefer mu.Unlock()":                 {{"drop_defer", ""}},
		"\tdefer resp.Body.Close() // free":   {{"drop_defer", ""}},
		"\tdefer cancel()":                    nil,
		"\tdefer os.RemoveAll(dir)":           nil,
		"\t// defer mu.Unlock()":              nil,
	} {
		var got []mut
		for _, m := range lineMutations(text) {
			got = append(got, mut{m.Operator, m.Mutated})
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("lineMutations(%q) = %v, want %v", text, got, want)
		}
	}
}

// seedDiff adds a Go file with a guard, a test file and a doc, in -U0 form.
const seedDiff = `diff --git a/pkg/a.go b/pkg/a.go
--- a/pkg/a.go
+++ b/pkg/a.go
@@ -3,0 +4,5 @@ func f() error {
+	err := g()
+	if err != nil {
+		return err
+	}
+	return h(!ok)
diff --git a/pkg/a_test.go b/pkg/a_test.go
--- /dev/null
+++ b/pkg/a_test.go
@@ -0,0 +1 @@
+	if a == b {
diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1 @@
-old
+if a == b
`

// Only non-test Go lines the PR added are candidates, numbered on the new side, in diff order.
func TestMutationCandidates(t *testing.T) {
	added := parseAddedLines(seedDiff)
	if len(added) != 7 || added[0] != (addedLine{"pkg/a.go", 4, "\terr := g()"}) || added[5] != (addedLine{"pkg/a_test.go", 1, "\tif a == b {"}) {
		t.Fatalf("added = %+v", added)
	}
	type c struct {
		Line     int
		Operator string
		end      int
	}
	var got []c
	for _, d := range mutationCandidates(added) {
		if d.Path != "pkg/a.go" {
			t.Errorf("candidate in %s, want only pkg/a.go", d.Path)
		}
		got = append(got, c{d.Line, d.Operator, d.end})
	}
	want := []c{{5, "flip_eq", 0}, {5, "drop_err_guard", 7}, {6, "return_nil", 0}, {8, "drop_not", 0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %+v, want %+v", got, want)
	}
}

// A subtle operator anywhere in the PR wins the pick; the old ones only seed a PR without one, drop_err_guard last.
func TestPickDefect(t *testing.T) {
	added := []addedLine{
		{"a.go", 1, "\tif a == b {"}, {"a.go", 2, "\tif err != nil {"}, {"a.go", 3, "\t}"},
		{"a.go", 4, "\tdefer mu.Unlock()"}, {"a.go", 5, "\tok := a && !b"}, {"c_test.go", 1, "\tx := s[i+1]"},
	}
	for _, tc := range []struct {
		cands []seededDefect
		want  []string
	}{
		{mutationCandidates(added), []string{"and_or", "drop_defer"}},
		{mutationCandidates(added[:3]), []string{"flip_eq"}},
		{[]seededDefect{{Operator: "drop_err_guard"}}, []string{"drop_err_guard"}},
	} {
		seen := map[string]bool{}
		for i := range 64 {
			id := fmt.Sprint("item-", i)
			d := pickDefect(tc.cands, id)
			if d != pickDefect(tc.cands, id) {
				t.Fatalf("pick for %s is not deterministic", id)
			}
			seen[d.Operator] = true
		}
		if got := slices.Sorted(maps.Keys(seen)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("picked operators = %v, want %v", got, tc.want)
		}
	}
}

func TestApplyDefect(t *testing.T) {
	src := "package a\n\nfunc f() error {\n\terr := g()\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n"
	got, err := applyDefect(src, seededDefect{Path: "a.go", Line: 6, Original: "\t\treturn err", Mutated: "\t\treturn nil"})
	if err != nil || got != strings.Replace(src, "\t\treturn err\n", "\t\treturn nil\n", 1) {
		t.Fatalf("one-line rewrite = %q, %v", got, err)
	}
	got, err = applyDefect(src, seededDefect{Path: "a.go", Line: 5, Original: "\tif err != nil {", end: 7})
	if err != nil || got != "package a\n\nfunc f() error {\n\terr := g()\n\treturn nil\n}\n" {
		t.Fatalf("guard deletion = %q, %v", got, err)
	}
	got, err = applyDefect(src, seededDefect{Path: "a.go", Line: 4, Original: "\terr := g()"})
	if err != nil || got != strings.Replace(src, "\terr := g()\n", "", 1) {
		t.Fatalf("one-line deletion = %q, %v", got, err)
	}
	if _, err := applyDefect(src, seededDefect{Path: "a.go", Line: 4, Original: "\tother"}); err == nil {
		t.Fatal("want an error when the line no longer reads as the diff said")
	}
}

func TestSeededHit(t *testing.T) {
	d := seededDefect{Path: "pkg/a.go", Line: 40}
	for _, tc := range []struct {
		name     string
		comments []vetting.ReviewComment
		want     float64
	}{
		{"on the line", []vetting.ReviewComment{{Path: "pkg/a.go", Line: 40}}, 1},
		{"edge of the window", []vetting.ReviewComment{{Path: "pkg/b.go", Line: 40}, {Path: "pkg/a.go", Line: 37}}, 1},
		{"near miss outside the window", []vetting.ReviewComment{{Path: "pkg/a.go", Line: 44}}, 0},
		{"wrong file", []vetting.ReviewComment{{Path: "pkg/b.go", Line: 40}}, 0},
		{"no comments", nil, 0},
	} {
		if got := seededHit(d, tc.comments); got != tc.want {
			t.Errorf("%s: seededHit = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@x.local"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// seedUpstream is a repo whose PR branch adds a guarded Go file and was then merged into main, as most
// exported items' PRs were: main alone can no longer show the PR's diff.
func seedUpstream(t *testing.T) (dir, base, head string) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir = t.TempDir()
	gitT(t, dir, "init", "--quiet", "--initial-branch=main")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", "package a\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "--quiet", "-m", "base")
	base = gitT(t, dir, "rev-parse", "HEAD")
	gitT(t, dir, "checkout", "--quiet", "-b", "feat/x")
	write("a.go", "package a\n\nfunc f(n int) error {\n\tif n == 0 {\n\t\treturn nil\n\t}\n\treturn g(n)\n}\n")
	gitT(t, dir, "commit", "--quiet", "-am", "add f")
	head = gitT(t, dir, "rev-parse", "HEAD")
	gitT(t, dir, "checkout", "--quiet", "main")
	gitT(t, dir, "merge", "--quiet", "--no-ff", "-m", "merge", "feat/x")
	return dir, base, head
}

// seedLangfuse serves one source item and records the seeded items written to the target dataset.
func seedLangfuse(t *testing.T, item map[string]any) (*httptest.Server, map[string]map[string]any) {
	created := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/public/dataset-items":
			writeJSON(w, map[string]any{"data": []any{item}, "meta": map[string]any{"totalPages": 1}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/public/v2/datasets/seeded":
			writeJSON(w, map[string]any{"name": "seeded"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/dataset-items":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			created[body["id"].(string)] = body
			writeJSON(w, map[string]any{"id": body["id"]})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, created
}

// A merged PR's item seeds into its own repo whose main is the PR's base and whose head branch is the PR's
// head with one planted defect folded in, so base...head shows the PR's diff plus the defect and nothing else.
func TestRunDatasetSeed(t *testing.T) {
	upstream, base, head := seedUpstream(t)
	files, _ := json.Marshal([]map[string]any{{"filename": "a.go", "patch": "old", "additions": 6}})
	item := map[string]any{
		"id":       "src-1",
		"metadata": map[string]any{"chat_id": "c1"},
		"input": map[string]any{
			"task":      "review " + head,
			"setup":     map[string]any{"Repo": "file://" + upstream, "BaseRef": "main", "ExistingHeadRef": "feat/x", "HeadSHA": head},
			"artifacts": map[string]any{"pull": `{"base":{"sha":"` + base + `"},"head":{"sha":"` + head + `"}}`, "files": string(files)},
		},
	}
	srv, created := seedLangfuse(t, item)
	mirror := filepath.Join(t.TempDir(), "upstream.git")
	opts := SeedOpts{Source: "src", Target: "seeded", Mirror: mirror}

	res, err := RunDatasetSeed(context.Background(), newTestClient(t, srv), opts)
	if err != nil || len(res) != 1 || res[0].Skipped != "" {
		t.Fatalf("RunDatasetSeed = %+v, %v", res, err)
	}
	var got struct {
		Input    datasetItemInput
		Metadata struct {
			Seeded     seededDefect
			ChatID     string `json:"chat_id"`
			SourceItem string `json:"source_item"`
		}
	}
	if err := remarshal(created[res[0].ItemID], &got); err != nil {
		t.Fatal(err)
	}
	d, s := got.Metadata.Seeded, got.Input.Setup
	repo := filepath.Join(filepath.Dir(mirror), "items", "src-1.git")
	if s.Repo != "file://"+repo || s.BaseRef != "main" || s.ExistingHeadRef != "feat/x" || s.HeadSHA == head ||
		got.Input.Task != "review "+s.HeadSHA || got.Metadata.ChatID != "c1" || got.Metadata.SourceItem != "src-1" {
		t.Fatalf("seeded item = %+v (setup %+v)", got, s)
	}
	// The PR's only mutable line: the seed is deterministic, whatever the item id hashes to.
	if d != (seededDefect{Path: "a.go", Line: 4, Operator: "flip_eq", Original: "\tif n == 0 {", Mutated: "\tif n != 0 {"}) {
		t.Fatalf("seeded defect = %+v", d)
	}
	diff := gitT(t, repo, "diff", "main..."+s.HeadSHA)
	if !strings.Contains(diff, "+func f(n int) error {") || !strings.Contains(diff, "+\tif n != 0 {") || strings.Contains(diff, "n == 0") {
		t.Fatalf("base...seeded diff misses the PR or the defect:\n%s", diff)
	}
	if gitT(t, repo, "rev-parse", "feat/x") != s.HeadSHA || gitT(t, repo, "log", "-1", "--format=%s", s.HeadSHA) != "add f" {
		t.Fatal("head branch is not the seeded rewrite of the PR head")
	}
	var fl []map[string]any
	_ = json.Unmarshal([]byte(got.Input.Artifacts["files"]), &fl)
	if p, _ := fl[0]["patch"].(string); !strings.HasPrefix(p, "@@ ") || strings.Contains(got.Input.Artifacts["pull"], head) {
		t.Fatalf("artifacts not regenerated for the seeded head: files=%v pull=%s", fl, got.Input.Artifacts["pull"])
	}

	again, err := RunDatasetSeed(context.Background(), newTestClient(t, srv), opts)
	if err != nil || again[0].ItemID != res[0].ItemID || !strings.Contains(created[res[0].ItemID]["input"].(map[string]any)["task"].(string), s.HeadSHA) {
		t.Fatalf("re-seed = %+v, %v; want the same item and seeded sha", again, err)
	}
}

// An item with no pinned head or pull base is skipped, not failed: the rest of the dataset still seeds.
func TestRunDatasetSeed_SkipsUnseedable(t *testing.T) {
	srv, created := seedLangfuse(t, map[string]any{"id": "src-2", "input": map[string]any{"task": "t"}})
	res, err := RunDatasetSeed(context.Background(), newTestClient(t, srv), SeedOpts{Source: "src", Target: "seeded", Mirror: t.TempDir()})
	if err != nil || len(res) != 1 || res[0].Skipped == "" || len(created) != 0 {
		t.Fatalf("res = %+v, err = %v, created = %v", res, err, created)
	}
}
