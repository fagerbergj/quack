package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// PromptBlock renders deployment facts for coding agents' prompts. Toolchains are probed via
// ResolveExecutable, as checks resolve them, so it never claims one that can't run.
func PromptBlock(caps Caps, checkCommands []string) string {
	lines := []string{fmt.Sprintf("%s %s. %s", osName(), archName(), sandboxLine(caps.Sandbox))}
	if tc := toolchainLine(caps); tc != "" {
		lines = append(lines, tc)
	}
	if line := notableAbsentLine(caps); line != "" {
		lines = append(lines, line)
	}
	if len(checkCommands) > 0 {
		lines = append(lines, "Check commands allowed: "+strings.Join(checkCommands, ", ")+".")
	}
	if caps.Limits.AddressSpaceMB > 0 {
		lines = append(lines, fmt.Sprintf("Address space limit: %d MB per process.", caps.Limits.AddressSpaceMB))
	}
	if len(caps.BuildDirs) > 0 {
		lines = append(lines, "If your working directory is read-only, these build-output dirs stay writable "+
			"when the repo's own .gitignore already ignores them (run installs/builds there directly instead of "+
			"copying the tree elsewhere): "+strings.Join(caps.BuildDirs, ", ")+".")
	}
	return strings.Join(lines, "\n")
}

func osName() string {
	if runtime.GOOS == "linux" {
		return "Linux"
	}
	return runtime.GOOS
}

func archName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	default:
		return runtime.GOARCH
	}
}

// sandboxLine must never claim network denial: neither sandbox mode unshares the network namespace.
func sandboxLine(mode SandboxMode) string {
	switch mode {
	case SandboxBwrap:
		return "Sandbox: bwrap (filesystem confined to your working directory and an isolated $HOME; system dirs read-only)."
	case SandboxLandlock:
		return "Sandbox: landlock (filesystem confined to your working directory and an isolated $HOME; system dirs read-only)."
	default:
		return "Sandbox: none (no OS-level isolation; full filesystem access)."
	}
}

// promptToolchains is probed via ResolveExecutable, the lookup checks use, so a listed one can run.
var promptToolchains = []struct {
	bin     string
	argv    []string
	extract func(output string) string
}{
	{"go", []string{"version"}, extractGoVersion},
	{"node", []string{"--version"}, extractMajorMinor("node")},
	{"python3", []string{"--version"}, extractMajorMinor("python")},
}

const toolchainProbeTimeout = 3 * time.Second

var versionNumRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

func extractGoVersion(out string) string {
	m := versionNumRe.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	v := "go" + m[1] + "." + m[2]
	if m[3] != "" {
		v += "." + m[3]
	}
	return v
}

func extractMajorMinor(label string) func(string) string {
	return func(out string) string {
		m := versionNumRe.FindStringSubmatch(out)
		if m == nil {
			return ""
		}
		return label + " " + m[1] + "." + m[2]
	}
}

// toolchainLine is "" when no probe resolved: a fact, not an error.
func toolchainLine(caps Caps) string {
	var items []string
	for _, tc := range promptToolchains {
		bin, err := ResolveExecutable("", tc.bin)
		if err != nil {
			continue
		}
		// Bounded: a hung startup probe would block the server from ever serving.
		ctx, cancel := context.WithTimeout(context.Background(), toolchainProbeTimeout)
		out, err := exec.CommandContext(ctx, bin, tc.argv...).CombinedOutput()
		cancel()
		if err != nil {
			continue
		}
		if s := tc.extract(string(out)); s != "" {
			items = append(items, s)
		}
	}
	if s := javaToolchain(caps); s != "" {
		items = append(items, s)
	}
	if s := androidToolchain(caps); s != "" {
		items = append(items, s)
	}
	if len(items) == 0 {
		return ""
	}
	return "Toolchains on PATH: " + strings.Join(items, ", ") + "."
}

// notableCLIs are commands agents reach for that the sandboxed runtime image (Dockerfile) omits.
var notableCLIs = []string{"curl", "gh"}

// notableAbsentLine names missing notableCLIs, sandboxed only - a dev host's ad-hoc gaps aren't a deployment fact.
func notableAbsentLine(caps Caps) string {
	if !EnforcesBoundary(caps.Sandbox) {
		return ""
	}
	var absent []string
	for _, bin := range notableCLIs {
		if !childPathHasExecutable(caps, bin) {
			absent = append(absent, bin)
		}
	}
	if len(absent) == 0 {
		return ""
	}
	return "Not on PATH: " + strings.Join(absent, ", ") + "."
}

// childPathHasExecutable checks ChildPath(caps), the agent child's own PATH - never the
// server's ambient one, since an absence claim must be true of what the spawned agent actually sees.
func childPathHasExecutable(caps Caps, bin string) bool {
	for _, dir := range strings.Split(ChildPath(caps), ":") {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(dir, bin)); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

var javaReleaseVersionRe = regexp.MustCompile(`JAVA_VERSION="?(\d+)`)

// javaToolchain reads JAVA_HOME's `release` file, with no subprocess; a JDK without one gets no line.
func javaToolchain(caps Caps) string {
	home := caps.Env["JAVA_HOME"]
	if home == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, "release"))
	if err != nil {
		return ""
	}
	m := javaReleaseVersionRe.FindStringSubmatch(string(raw))
	if m == nil {
		return ""
	}
	return fmt.Sprintf("jdk%s (JAVA_HOME=%s)", m[1], home)
}

var androidPlatformRe = regexp.MustCompile(`^android-(\d+)$`)

// androidToolchain reads <ANDROID_HOME or ANDROID_SDK_ROOT>/platforms; sdkmanager may be absent even with an SDK.
func androidToolchain(caps Caps) string {
	key := "ANDROID_HOME"
	home := caps.Env[key]
	if home == "" {
		key = "ANDROID_SDK_ROOT"
		home = caps.Env[key]
	}
	if home == "" {
		return ""
	}
	entries, err := os.ReadDir(filepath.Join(home, "platforms"))
	if err != nil {
		return ""
	}
	best := -1
	for _, e := range entries {
		m := androidPlatformRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if n, convErr := strconv.Atoi(m[1]); convErr == nil && n > best {
			best = n
		}
	}
	if best < 0 {
		return ""
	}
	return fmt.Sprintf("Android SDK %d (%s=%s)", best, key, home)
}
