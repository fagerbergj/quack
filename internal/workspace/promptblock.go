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

// PromptBlock renders the deployment-constant workspace/toolchain facts
// injected into every coding agent's system prompt (#663): OS, sandbox mode,
// toolchains actually present, the check-command allowlist, and the
// per-process address-space limit. Generated from caps/checkCommands at
// startup - never hand-written - so it can never claim a toolchain that
// isn't actually there: presence is PROBED the same way a derived check
// would resolve it (ResolveExecutable), not trusted from config. A toolchain
// that fails its probe simply has no line, so an agent that needs it says it
// could not verify rather than "verifying" against a tool it can't run.
func PromptBlock(caps Caps, checkCommands []string) string {
	lines := []string{fmt.Sprintf("%s %s. %s", osName(), archName(), sandboxLine(caps.Sandbox))}
	if tc := toolchainLine(caps); tc != "" {
		lines = append(lines, tc)
	}
	if len(checkCommands) > 0 {
		lines = append(lines, "Check commands allowed: "+strings.Join(checkCommands, ", ")+".")
	}
	if caps.Limits.AddressSpaceMB > 0 {
		lines = append(lines, fmt.Sprintf("Address space limit: %d MB per process.", caps.Limits.AddressSpaceMB))
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

// sandboxLine states the actual OS boundary (or its absence) rather than the
// aspirational one - e.g. neither bwrap nor landlock here unshares the
// network namespace (agents legitimately run `npm ci`/`go mod download`), so
// this must never claim network denial.
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

// promptToolchains is the fixed catalog of language runtimes probed on the
// server's own ambient PATH for the "Toolchains on PATH" line - resolved via
// ResolveExecutable, the exact lookup a derived check (vetting.toolchainPresent)
// will use, so this can never list one that a check would then fail to run.
var promptToolchains = []struct {
	bin     string
	argv    []string
	extract func(output string) string
}{
	{"go", []string{"version"}, extractGoVersion},
	{"node", []string{"--version"}, extractMajorMinor("node")},
	{"python3", []string{"--version"}, extractMajorMinor("python")},
}

// toolchainProbeTimeout bounds each startup version probe.
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

// toolchainLine renders "Toolchains on PATH: …" from every probe that
// actually resolved, or "" when none did - an empty catalog is a fact, not
// an error, on a host with none of these installed.
func toolchainLine(caps Caps) string {
	var items []string
	for _, tc := range promptToolchains {
		bin, err := ResolveExecutable("", tc.bin)
		if err != nil {
			continue
		}
		// Bounded: this runs at startup, and a hung probe would block the
		// server from ever serving (cf. #316 - a child holding the pipe makes
		// Wait block forever). An unprobeable toolchain is simply absent.
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

var javaReleaseVersionRe = regexp.MustCompile(`JAVA_VERSION="?(\d+)`)

// javaToolchain reports the JDK found at workspace.env's JAVA_HOME (Gradle's
// own lookup - config/quack.yaml), via the `release` file every OpenJDK/
// Adoptium/Zulu distribution ships (JAVA_VERSION="17.0.9…") - deterministic,
// no subprocess needed. A JAVA_HOME with no release file gets no line: a
// custom build we can't version is not one we can safely claim either.
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

// androidToolchain reports the highest installed platform under
// workspace.env's ANDROID_HOME (falling back to ANDROID_SDK_ROOT - AGP
// accepts either, config/quack.yaml sets both to the same path) by reading
// <home>/platforms - no `sdkmanager` subprocess, which isn't guaranteed
// present even when the SDK is.
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
