package workspace

import "time"

// Caps bounds every workspace tool call. Read-shaped results truncate loudly.
type Caps struct {
	MaxReadBytes   int64
	MaxWriteBytes  int64
	MaxResults     int
	MaxListEntries int
	Timeout        time.Duration
	// ponytail: git predates this and keeps its own maxGitOutputBytes.
	MaxOutputBytes int64
	ExtraPath      []string
	Env            map[string]string
	HomeDir        string
	// ScratchDir is a per-node TMPDIR (Jail.ScratchDir); "" falls back to the shared HomeDir/tmp.
	ScratchDir string
	// ACPStateDir is exposed to an ACP child as PI_ACP_STATE_DIR; "" omits it.
	ACPStateDir string
	WorkRoot    string
	Sandbox     SandboxMode
	Limits      Limits
	ExtraRO     []string
	// ReadOnly mounts the node's own work/dir RO, enforcing the agent's read_only config.
	ReadOnly bool
	// BuildDirs stay writable on a ReadOnly node when the repo's .gitignore already ignores them,
	// so a read-only reviewer can build in place.
	BuildDirs []string
}

func DefaultCaps() Caps {
	return Caps{
		MaxReadBytes:   256 * 1024,
		MaxWriteBytes:  2 * 1024 * 1024,
		MaxResults:     200,
		MaxListEntries: 500,
		Timeout:        60 * time.Second,
		MaxOutputBytes: 64 * 1024,
	}
}

// IsZero exists because Caps contains slices and maps, so == doesn't compile.
func (c Caps) IsZero() bool {
	return c.MaxReadBytes == 0 && c.MaxWriteBytes == 0 && c.MaxResults == 0 &&
		c.MaxListEntries == 0 && c.Timeout == 0 && c.MaxOutputBytes == 0 &&
		len(c.ExtraPath) == 0 && len(c.Env) == 0 && c.HomeDir == "" && c.ScratchDir == "" && c.ACPStateDir == "" && c.Sandbox == "" && c.Limits == Limits{} &&
		len(c.ExtraRO) == 0 && !c.ReadOnly && len(c.BuildDirs) == 0
}
