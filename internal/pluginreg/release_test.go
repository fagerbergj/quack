package pluginreg

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/pluginreg/pluginregtest"
)

func TestReleaseBinaryVerifiesAgainstTheReleaseChecksum(t *testing.T) {
	asset := fmt.Sprintf("quack-ext-x-%s-%s", runtime.GOOS, runtime.GOARCH)
	good, evil := []byte("#!/bin/sh\necho good\n"), []byte("#!/bin/sh\necho evil\n")
	srv := pluginregtest.ReleaseServer(t, map[string][]byte{
		"plugin/x/v1/" + asset:   good,
		"plugin/x/v1/SHA256SUMS": pluginregtest.SHA256SUMS(map[string][]byte{asset: good}),
	})
	old := ReleaseURL
	ReleaseURL = func(_, _, tag, asset string) string { return srv.URL + "/" + tag + "/" + asset }
	t.Cleanup(func() { ReleaseURL = old })
	root := t.TempDir()
	row := Plugin{Name: "x", Source: SourceGitHub, Owner: "o", Repo: "r", Ref: "plugin/x/v1"}

	bin, err := ReleaseBinary(context.Background(), root, row)
	if err != nil {
		t.Fatalf("ReleaseBinary(v1) = %v", err)
	}
	if b, _ := os.ReadFile(bin); string(b) != string(good) {
		t.Errorf("v1 binary = %q, want the released bytes", b)
	}
	if fi, _ := os.Stat(bin); fi.Mode()&0o111 == 0 {
		t.Errorf("v1 binary mode %v is not executable", fi.Mode())
	}
	srv.Close() // a verified cache needs no network
	if again, err := ReleaseBinary(context.Background(), root, row); err != nil || again != bin {
		t.Errorf("cached ReleaseBinary = %q, %v; want %q", again, err, bin)
	}

	srv = pluginregtest.ReleaseServer(t, map[string][]byte{
		"plugin/x/v2/" + asset:   evil,
		"plugin/x/v2/SHA256SUMS": pluginregtest.SHA256SUMS(map[string][]byte{asset: good}),
		"plugin/x/v3/SHA256SUMS": pluginregtest.SHA256SUMS(map[string][]byte{"other": good}),
	})
	for ref, want := range map[string]string{"plugin/x/v2": "does not match", "plugin/x/v3": "not listed", "": "not pinned"} {
		row.Ref = ref
		if bin, err := ReleaseBinary(context.Background(), root, row); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ReleaseBinary(%q) = %q, %v; want an error containing %q", ref, bin, err, want)
		}
	}
	if entries, _ := os.ReadDir(root + "/x/bin/plugin_x_v2"); len(entries) != 0 {
		t.Errorf("a mismatched download left %d files behind", len(entries))
	}
}
