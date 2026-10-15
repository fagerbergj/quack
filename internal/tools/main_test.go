package tools

import (
	"os"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestMain answers the __sandbox-exec self-exec a confined git call makes of this test binary.
func TestMain(m *testing.M) {
	workspace.RunSandboxExecIfInvoked()
	workspace.GitProtocol = "file" // fixtures are local bare repos
	// A per-run deadline would only measure host load; go test -timeout bounds a real hang.
	vetting.MermaidValidateTimeout = time.Hour
	os.Exit(m.Run())
}
