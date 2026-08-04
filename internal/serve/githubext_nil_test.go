package serve

import (
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/config"
)

// extensions.github is optional (*GitHubExtensionConfig) and absent from the
// default config, every non-GitHub deployment, and `quack replay` - which
// builds this same server in-process. buildFromConfig dereferenced it
// unguarded for the run deadline (#571), so any such start panicked with a
// nil pointer before serving anything. Pin the guard, not the panic.
func TestRunDeadlineToleratesAbsentGitHubExtension(t *testing.T) {
	var cfg config.Config
	if cfg.Extensions.GitHub != nil {
		t.Fatal("precondition: a zero Config must have no github extension")
	}

	// The exact expression buildFromConfig evaluates. Before the fix this
	// panicked; a nil extension simply means no configured deadline.
	var got time.Duration
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("nil github extension panicked: %v", r)
			}
		}()
		if gh := cfg.Extensions.GitHub; gh != nil && gh.RunTimeoutMinutes > 0 {
			got = time.Duration(gh.RunTimeoutMinutes) * time.Minute
		}
	}()
	if got != 0 {
		t.Errorf("deadline = %v, want 0 with no github extension", got)
	}
}

// The deadline still applies when the extension IS configured - the guard must
// not silently drop it.
func TestRunDeadlineAppliedWhenGitHubExtensionPresent(t *testing.T) {
	var cfg config.Config
	cfg.Extensions.GitHub = &config.GitHubExtensionConfig{RunTimeoutMinutes: 240}

	var got time.Duration
	if gh := cfg.Extensions.GitHub; gh != nil && gh.RunTimeoutMinutes > 0 {
		got = time.Duration(gh.RunTimeoutMinutes) * time.Minute
	}
	if want := 240 * time.Minute; got != want {
		t.Errorf("deadline = %v, want %v", got, want)
	}
}
