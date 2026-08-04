package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeBinary drops an executable shell script named name into dir,
// printing output to stdout when run - a stand-in for a probed toolchain
// binary (mirrors internal/vetting/checks_test.go's convention).
func writeFakeBinary(t *testing.T, dir, name, output string) {
	t.Helper()
	script := "#!/bin/sh\necho '" + output + "'\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

func TestPromptBlockSandboxLine(t *testing.T) {
	cases := []struct {
		mode SandboxMode
		want string
	}{
		{SandboxBwrap, "Sandbox: bwrap ("},
		{SandboxLandlock, "Sandbox: landlock ("},
		{SandboxNone, "Sandbox: none (no OS-level isolation"},
		{"", "Sandbox: none (no OS-level isolation"}, // zero value = no boundary, must say so
	}
	for _, c := range cases {
		t.Setenv("PATH", t.TempDir()) // no toolchains resolvable - isolates this to the sandbox line
		got := PromptBlock(Caps{Sandbox: c.mode}, nil)
		if !strings.Contains(got, c.want) {
			t.Errorf("Sandbox %q: got %q, want to contain %q", c.mode, got, c.want)
		}
	}
}

func TestPromptBlockNeverClaimsNetworkDenial(t *testing.T) {
	// Neither sandbox mode unshares the network namespace (sandbox.go) - a
	// prompt claiming otherwise is exactly the silent-degradation shape #663
	// exists to prevent.
	t.Setenv("PATH", t.TempDir())
	for _, mode := range []SandboxMode{SandboxBwrap, SandboxLandlock, SandboxNone} {
		got := PromptBlock(Caps{Sandbox: mode}, nil)
		if strings.Contains(strings.ToLower(got), "network denied") {
			t.Errorf("Sandbox %q: prompt block falsely claims network denial: %q", mode, got)
		}
	}
}

func TestPromptBlockCheckCommandsExactlyConfig(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	commands := []string{"go build", "go vet", "npm run", "./gradlew"}
	got := PromptBlock(Caps{}, commands)
	want := "Check commands allowed: go build, go vet, npm run, ./gradlew."
	if !strings.Contains(got, want) {
		t.Errorf("got %q, want to contain exactly %q", got, want)
	}
}

func TestPromptBlockCheckCommandsAbsentWhenEmpty(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got := PromptBlock(Caps{}, nil)
	if strings.Contains(got, "Check commands allowed") {
		t.Errorf("empty check_commands should render no line, got %q", got)
	}
}

func TestPromptBlockAddressSpaceLimit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got := PromptBlock(Caps{Limits: Limits{AddressSpaceMB: 8192}}, nil)
	if !strings.Contains(got, "Address space limit: 8192 MB per process.") {
		t.Errorf("got %q, want the address-space limit line", got)
	}
	got = PromptBlock(Caps{}, nil)
	if strings.Contains(got, "Address space limit") {
		t.Errorf("unset limit should render no line, got %q", got)
	}
}

// TestPromptBlockToolchainRemovalRemovesLine is the core #663 assertion: a
// toolchain absent from what's actually resolvable never appears, and
// removing it (here: a PATH with no `go` on it) removes exactly its line,
// nothing else.
func TestPromptBlockToolchainRemovalRemovesLine(t *testing.T) {
	dir := t.TempDir()
	writeFakeBinary(t, dir, "go", "go version go1.24.2 linux/amd64")
	t.Setenv("PATH", dir)

	present := PromptBlock(Caps{}, nil)
	if !strings.Contains(present, "go1.24.2") {
		t.Fatalf("with go on PATH, want go1.24.2 in the toolchain line, got %q", present)
	}

	// Remove it from the resolvable set (config: exec_path effectively empty,
	// server PATH has nothing) - the whole line must disappear, since go was
	// the only toolchain present.
	t.Setenv("PATH", t.TempDir())
	absent := PromptBlock(Caps{}, nil)
	if strings.Contains(absent, "Toolchains on PATH") {
		t.Errorf("with no toolchains resolvable, the whole line should be absent, got %q", absent)
	}
}

// TestPromptBlockAstGrepRemovalRemovesLine mirrors
// TestPromptBlockToolchainRemovalRemovesLine for ast-grep specifically (#684):
// the gate's own use of it is a separate, allowlist-gated path
// (vetting.packageDeclarationCriterion) - this only covers the coding
// agents' PROMPT line, which must vanish exactly when the binary isn't
// actually resolvable, never claim a tool a round can't run.
func TestPromptBlockAstGrepRemovalRemovesLine(t *testing.T) {
	dir := t.TempDir()
	writeFakeBinary(t, dir, "ast-grep", "ast-grep 0.45.0")
	t.Setenv("PATH", dir)

	present := PromptBlock(Caps{}, nil)
	if !strings.Contains(present, "ast-grep 0.45") {
		t.Fatalf("with ast-grep on PATH, want ast-grep 0.45 in the toolchain line, got %q", present)
	}

	t.Setenv("PATH", t.TempDir())
	absent := PromptBlock(Caps{}, nil)
	if strings.Contains(absent, "ast-grep") {
		t.Errorf("with ast-grep not resolvable, want no mention of it, got %q", absent)
	}
}

func TestPromptBlockMultipleToolchains(t *testing.T) {
	dir := t.TempDir()
	writeFakeBinary(t, dir, "go", "go version go1.24.2 linux/amd64")
	writeFakeBinary(t, dir, "node", "v22.14.0")
	writeFakeBinary(t, dir, "python3", "Python 3.12.3")
	t.Setenv("PATH", dir)

	got := PromptBlock(Caps{}, nil)
	for _, want := range []string{"go1.24.2", "node 22.14", "python 3.12"} {
		if !strings.Contains(got, want) {
			t.Errorf("got %q, want to contain %q", got, want)
		}
	}
}

func TestPromptBlockJavaHomeToolchain(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "release"), []byte(`JAVA_VERSION="17.0.9"`+"\n"), 0o644); err != nil {
		t.Fatalf("write release file: %v", err)
	}

	got := PromptBlock(Caps{Env: map[string]string{"JAVA_HOME": home}}, nil)
	want := "jdk17 (JAVA_HOME=" + home + ")"
	if !strings.Contains(got, want) {
		t.Errorf("got %q, want to contain %q", got, want)
	}

	// Removing JAVA_HOME from config removes the line, not just the version.
	got = PromptBlock(Caps{}, nil)
	if strings.Contains(got, "jdk") {
		t.Errorf("with no JAVA_HOME configured, want no jdk line, got %q", got)
	}
}

// TestPromptBlockJavaHomeWithoutReleaseFileOmitted asserts the exact failure
// mode #663 exists to prevent: a configured-but-unverifiable toolchain must
// never appear, even though the operator DID set JAVA_HOME.
func TestPromptBlockJavaHomeWithoutReleaseFileOmitted(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir() // no release file inside
	got := PromptBlock(Caps{Env: map[string]string{"JAVA_HOME": home}}, nil)
	if strings.Contains(got, "jdk") {
		t.Errorf("JAVA_HOME with no readable version should render no line, got %q", got)
	}
}

func TestPromptBlockAndroidHomeToolchain(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	platforms := filepath.Join(home, "platforms")
	if err := os.MkdirAll(filepath.Join(platforms, "android-33"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(platforms, "android-35"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := PromptBlock(Caps{Env: map[string]string{"ANDROID_HOME": home}}, nil)
	want := "Android SDK 35 (ANDROID_HOME=" + home + ")"
	if !strings.Contains(got, want) {
		t.Errorf("got %q, want to contain %q (highest platform wins)", got, want)
	}

	got = PromptBlock(Caps{}, nil)
	if strings.Contains(got, "Android SDK") {
		t.Errorf("with no ANDROID_HOME configured, want no Android SDK line, got %q", got)
	}
}

func TestPromptBlockAndroidSdkRootFallback(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "platforms", "android-35"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := PromptBlock(Caps{Env: map[string]string{"ANDROID_SDK_ROOT": home}}, nil)
	want := "Android SDK 35 (ANDROID_SDK_ROOT=" + home + ")"
	if !strings.Contains(got, want) {
		t.Errorf("got %q, want to contain %q (labelled by the key actually set)", got, want)
	}
}

func TestPromptBlockOSAndArch(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got := PromptBlock(Caps{}, nil)
	if !strings.HasPrefix(got, osName()+" "+archName()+". Sandbox:") {
		t.Errorf("got %q, want it to start with the OS/arch line", got)
	}
}
