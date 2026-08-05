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
	WorkRoot       string
	Sandbox        SandboxMode
	Limits         Limits
	ExtraRO        []string
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

// IsZero reports all fields unset. Needed because Caps contains slices/maps.
func (c Caps) IsZero() bool {
	return c.MaxReadBytes == 0 && c.MaxWriteBytes == 0 && c.MaxResults == 0 &&
		c.MaxListEntries == 0 && c.Timeout == 0 && c.MaxOutputBytes == 0 &&
		len(c.ExtraPath) == 0 && len(c.Env) == 0 && c.HomeDir == "" && c.Sandbox == "" && c.Limits == Limits{} &&
		len(c.ExtraRO) == 0
}
