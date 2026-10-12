package sleeper

import "testing"

// testExtension wires the qa-mock fixture client with the owner's real
// default_user/default_league, so tools exercise their fallback path too.
func testExtension(t *testing.T) *extension {
	t.Helper()
	return &extension{
		client: newTestClient(t),
		cfg:    config{DefaultUser: testUser, DefaultLeague: testLeague},
	}
}
