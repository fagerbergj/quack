package pgtest

import (
	"database/sql"
	"testing"
)

// Two DSNs from the one container must be separate databases, or shared-container tests would see each
// other's rows.
func TestDSN_IsolatesDatabases(t *testing.T) {
	a, err := sql.Open("pgx", DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	b, err := sql.Open("pgx", DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if _, err := a.Exec("CREATE TABLE only_in_a (id int)"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_name = 'only_in_a'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("table created through one DSN is visible through the other (count %d)", n)
	}
}
