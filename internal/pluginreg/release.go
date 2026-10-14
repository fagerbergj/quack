package pluginreg

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ReleaseURL is where a release asset downloads from; tests point it at a fake release server.
var ReleaseURL = func(owner, repo, tag, asset string) string {
	return fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s", owner, repo, tag, asset)
}

// releaseTimeout bounds one release fetch; maxBinary caps a download, which only the checksum otherwise bounds.
const (
	releaseTimeout = 2 * time.Minute
	maxBinary      = 512 << 20
	sumsAsset      = "SHA256SUMS"
)

// ReleaseBinary is the path of quack-ext-<name>-<os>-<arch> from the release at p's pinned tag, downloaded once
// under root and checked against that release's SHA256SUMS on every call: nothing unverified is returned.
func ReleaseBinary(ctx context.Context, root string, p Plugin) (string, error) {
	tagDir := strings.ReplaceAll(p.Ref, "/", "_")
	if p.Source != SourceGitHub || p.Ref == "" || !filepath.IsLocal(tagDir) {
		return "", fmt.Errorf("plugin %q is not pinned to a github release tag", p.Name)
	}
	asset := fmt.Sprintf("quack-ext-%s-%s-%s", p.Name, runtime.GOOS, runtime.GOARCH)
	dir := filepath.Join(root, p.Name, "bin", tagDir)
	bin, sumsPath := filepath.Join(dir, asset), filepath.Join(dir, sumsAsset)
	if sums, err := os.ReadFile(sumsPath); err == nil && verifyFile(bin, sums, asset) == nil {
		return bin, nil
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	var sums bytes.Buffer
	err := download(ctx, ReleaseURL(p.Owner, p.Repo, p.Ref, sumsAsset), &sums, 1<<20)
	var want string
	if err == nil {
		want, err = listedSum(sums.Bytes(), asset)
	}
	if err == nil {
		err = os.MkdirAll(dir, 0o755)
	}
	if err == nil {
		err = fetchVerified(ctx, ReleaseURL(p.Owner, p.Repo, p.Ref, asset), bin, want)
	}
	if err != nil {
		return "", fmt.Errorf("plugin %s release %s: %w", p.Name, p.Ref, err)
	}
	return bin, os.WriteFile(sumsPath, sums.Bytes(), 0o644)
}

// fetchVerified downloads url beside bin and renames it into place only once its sha256 is want.
func fetchVerified(ctx context.Context, url, bin, want string) error {
	f, err := os.CreateTemp(filepath.Dir(bin), ".download-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	h := sha256.New()
	err = download(ctx, url, io.MultiWriter(f, h), maxBinary)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = matchSum(h.Sum(nil), want)
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o755)
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), bin)
}

func download(ctx context.Context, url string, w io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	_, err = io.Copy(w, io.LimitReader(resp.Body, limit))
	return err
}

func verifyFile(path string, sums []byte, asset string) error {
	want, err := listedSum(sums, asset)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	return matchSum(h.Sum(nil), want)
}

// listedSum is asset's sha256 in a sha256sum-format file.
func listedSum(sums []byte, asset string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			return f[0], nil
		}
	}
	return "", errors.New(asset + " is not listed in the release's " + sumsAsset)
}

func matchSum(got []byte, want string) error {
	if !strings.EqualFold(hex.EncodeToString(got), want) {
		return fmt.Errorf("sha256 %x does not match the release's %s", got, want)
	}
	return nil
}
