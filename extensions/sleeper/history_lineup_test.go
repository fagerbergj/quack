package sleeper

import "github.com/fagerbergj/quack-extensions/sleeper/sleepergen"

// playerDumpAt builds a minimal players dump from alternating id/position
// pairs, for tests that only need position-aware lineup math.
func playerDumpAt(idsAndPositions ...string) map[string]sleepergen.Player {
	out := map[string]sleepergen.Player{}
	for i := 0; i+1 < len(idsAndPositions); i += 2 {
		id, pos := idsAndPositions[i], idsAndPositions[i+1]
		out[id] = sleepergen.Player{PlayerId: id, Position: &pos}
	}
	return out
}
