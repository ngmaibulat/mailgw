package migrate

import (
	"strings"
	"testing"
)

// The embedded set is the thing most likely to break silently: go:embed failing
// to match is a successful build and an empty migration run.
func TestNames_EmbedsEveryMigration(t *testing.T) {
	names, err := Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	// 26 as of the copy from the Bun service. This number is expected to grow;
	// what it must never do is shrink or drop to zero.
	if len(names) < 26 {
		t.Fatalf("embedded %d migrations, want at least 26 — did go:embed stop matching?", len(names))
	}
	if names[0] != "001_create_connection.sql" {
		t.Errorf("first migration = %q, want 001_create_connection.sql", names[0])
	}
}

// Lexicographic sort is apply order only because of the zero-padded prefix. A
// file named "27_x.sql" would sort before "001_..." and run first.
func TestNames_AreSortedAndPrefixed(t *testing.T) {
	names, err := Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	for i, n := range names {
		if len(n) < 4 || n[3] != '_' {
			t.Errorf("migration %q has no three-digit prefix; lexicographic sort would misorder it", n)
		}
		for j := 0; j < 3; j++ {
			if n[j] < '0' || n[j] > '9' {
				t.Errorf("migration %q has a non-numeric prefix", n)
				break
			}
		}
		if i > 0 && names[i-1] >= n {
			t.Errorf("migrations are not sorted: %q before %q", names[i-1], n)
		}
	}
}

// The tracking table must stay byte-compatible with the Bun runner's, because
// an existing database was migrated by that one and this one must see its rows.
func TestCreateTable_MatchesTheBunRunnerContract(t *testing.T) {
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS _migrations",
		"name      VARCHAR(255) NOT NULL UNIQUE",
		"appliedAt DATETIME     NOT NULL",
	} {
		if !strings.Contains(createTable, want) {
			t.Errorf("createTable is missing %q — an existing database would not be recognised", want)
		}
	}
}

func TestCheck_PassesWithTheEmbeddedSet(t *testing.T) {
	if err := Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
}
