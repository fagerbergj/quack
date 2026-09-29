package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// confineGit turns ConfineGit's Landlock confinement on for one test. It flips a process global, so callers
// never run in parallel.
func confineGit(t *testing.T) {
	t.Helper()
	requireLandlock(t)
	gitConfined.Store(true)
	t.Cleanup(func() { gitConfined.Store(false) })
}

func rawGit(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=q", "-c", "user.email=q@x.local"}, args...)
	out, err := exec.Command(bin, full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// victimAndClone: a victim repo (another chat's clone, its objects packed) and quack's clone, as siblings.
func victimAndClone(t *testing.T) (bin, victim, clone, sha string) {
	t.Helper()
	bin, clone = gitConfigFixture(t)
	rawGit(t, bin, clone, "commit", "--quiet", "--allow-empty", "-m", "clone")
	victim = t.TempDir()
	rawGit(t, bin, victim, "init", "--quiet", "--initial-branch=main")
	writeFile(t, filepath.Join(victim, "secret.txt"), "victim data\n")
	rawGit(t, bin, victim, "add", "secret.txt")
	rawGit(t, bin, victim, "commit", "--quiet", "-m", "victim")
	rawGit(t, bin, victim, "repack", "-a", "-d", "-q")
	rawGit(t, bin, victim, "prune-packed")
	return bin, victim, clone, rawGit(t, bin, victim, "rev-parse", "HEAD")
}

func symlinkOver(t *testing.T, target, link string) {
	t.Helper()
	if err := os.RemoveAll(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// treeListing names every file under dir with its size, to prove a tree untouched.
func treeListing(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %s %d\n", p, fi.Mode(), fi.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestConfinedGitDeniesSymlinkedObjects: an objects dir or pack dir symlinked at another repo is neither read nor
// written by quack's git.
func TestConfinedGitDeniesSymlinkedObjects(t *testing.T) {
	for link, write := range map[string][]string{
		"objects":      {"hash-object", "-w", "--stdin"},
		"objects/pack": {"repack", "-a", "-q"},
	} {
		t.Run(link, func(t *testing.T) {
			bin, victim, clone, sha := victimAndClone(t)
			symlinkOver(t, filepath.Join(victim, ".git", link), filepath.Join(clone, ".git", link))
			before := treeListing(t, filepath.Join(victim, ".git"))
			confineGit(t)
			if _, err := runGitCmd(t, bin, clone, "cat-file", "-e", sha); err == nil {
				t.Errorf("read another repo's object through a symlinked %s", link)
			}
			_, _ = runGitCmd(t, bin, clone, write...)
			if after := treeListing(t, filepath.Join(victim, ".git")); after != before {
				t.Errorf("git %v wrote into the other repo through a symlinked %s", write, link)
			}
		})
	}
}

// TestConfinedGitDeniesSymlinkedRefs: a loose ref or packed-refs symlinked at another repo does not resolve.
func TestConfinedGitDeniesSymlinkedRefs(t *testing.T) {
	for link, packRefs := range map[string]bool{"refs/heads/stolen": false, "packed-refs": true} {
		t.Run(link, func(t *testing.T) {
			bin, victim, clone, sha := victimAndClone(t)
			// git hides a ref whose object is missing, so give the clone the victim's objects: only the ref leaks.
			rawGit(t, bin, clone, "fetch", "--quiet", victim, "main")
			target := filepath.Join(victim, ".git", "refs", "heads", "main")
			if packRefs {
				rawGit(t, bin, victim, "pack-refs", "--all")
				target = filepath.Join(victim, ".git", "packed-refs")
			}
			symlinkOver(t, target, filepath.Join(clone, ".git", link))
			confineGit(t)
			if out, _ := runGitCmd(t, bin, clone, "show-ref"); strings.Contains(out, sha) {
				t.Errorf("resolved another repo's ref through a symlinked %s:\n%s", link, out)
			}
		})
	}
}

// TestConfinedGitDeniesLateAlternates: alternates written after GitCmd's check lead nowhere.
func TestConfinedGitDeniesLateAlternates(t *testing.T) {
	bin, victim, clone, sha := victimAndClone(t)
	confineGit(t)
	cmd, done, err := GitCmd(context.Background(), bin, clone, clone, []string{"cat-file", "-e", sha}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	writeFile(t, filepath.Join(clone, ".git", "objects", "info", "alternates"), filepath.Join(victim, ".git", "objects")+"\n")
	if err := cmd.Run(); err == nil {
		t.Error("read another repo's object through alternates added after the check")
	}
}

// TestConfinedGitLegitimateOps: clone, fetch and push against an explicitly granted local bare remote still work.
func TestConfinedGitLegitimateOps(t *testing.T) {
	bin, src := gitConfigFixture(t)
	rawGit(t, bin, src, "commit", "--quiet", "--allow-empty", "-m", "first")
	bare := filepath.Join(t.TempDir(), "remote.git")
	rawGit(t, bin, src, "clone", "--quiet", "--bare", src, bare)
	prev := GitProtocol
	GitProtocol = "file"
	t.Cleanup(func() { GitProtocol = prev })
	confineGit(t)
	run := func(clone, dir string, argv []string, rw ...string) {
		t.Helper()
		cmd, done, err := GitCmd(context.Background(), bin, clone, dir, argv, nil, rw...)
		if err != nil {
			t.Fatal(err)
		}
		defer done()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("confined git %v: %v\n%s", argv, err, out)
		}
	}
	clone := filepath.Join(t.TempDir(), "clone")
	run("", "", []string{"clone", "--quiet", "file://" + bare, clone}, clone, bare)
	run(clone, clone, []string{"-c", "user.email=q@x.local", "-c", "user.name=q", "commit", "--quiet", "--allow-empty", "-m", "second"})
	run(clone, clone, []string{"fetch", "--quiet", "file://" + bare, "HEAD"}, bare)
	run(clone, clone, []string{"push", "--quiet", "file://" + bare, "HEAD:refs/heads/pushed"}, bare)
	if got, want := rawGit(t, bin, bare, "rev-parse", "pushed"), rawGit(t, bin, clone, "rev-parse", "HEAD"); got != want {
		t.Errorf("bare remote's pushed = %s, want the clone's HEAD %s", got, want)
	}
}

// TestWorktreeCommonGitDirRefusesOddPointers: a FIFO or symlinked .git pointer yields no grant instead of blocking
// or being followed.
func TestWorktreeCommonGitDirRefusesOddPointers(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, ptr string){
		"fifo": func(t *testing.T, ptr string) {
			if err := os.Remove(ptr); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(ptr, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, ptr string) {
			moved := filepath.Join(t.TempDir(), "pointer")
			if err := os.Rename(ptr, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, ptr); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, wt := repoWithWorktree(t)
			ptr := filepath.Join(wt, ".git")
			if WorktreeCommonGitDir(wt) == "" {
				t.Fatal("fixture worktree has no common dir")
			}
			plant(t, ptr)
			if got := WorktreeCommonGitDir(wt); got != "" {
				t.Errorf("WorktreeCommonGitDir = %q, want none", got)
			}
		})
	}
}

// TestNewGitGrants: only a confined call gets grants; it creates its rw dirs and reads CA bundles and the askpass
// binary.
func TestNewGitGrants(t *testing.T) {
	home := t.TempDir()
	ConfineGit(SandboxNone)
	if g, err := newGitGrants("git", home, nil, nil); g != nil || err != nil {
		t.Errorf("unconfined call: grants %v, err %v", g, err)
	}
	ConfineGit(SandboxLandlock)
	t.Cleanup(func() { gitConfined.Store(false) })
	target := filepath.Join(t.TempDir(), "new", "clone")
	g, err := newGitGrants("git", home, []string{"GIT_SSL_CAINFO=/ca.pem", "GIT_ASKPASS=/jail/.quack-askpass"}, []string{target})
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Errorf("rw dir %s not created: %v", target, err)
	}
	if !slices.Contains(g.rw, target) || !slices.Contains(g.rw, home) || slices.Contains(g.ro, "/jail/.quack-askpass") ||
		!slices.Contains(g.ro, "/ca.pem") || !slices.Contains(g.ro, self) {
		t.Errorf("grants rw %v ro %v", g.rw, g.ro)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	writeFile(t, blocker, "")
	if _, _, err := GitCmd(context.Background(), "git", "", "", []string{"version"}, nil, filepath.Join(blocker, "sub")); err == nil {
		t.Error("rw dir under a file: want an error")
	}
}
