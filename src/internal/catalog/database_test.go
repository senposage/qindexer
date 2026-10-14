package catalog

import "testing"

func TestPostgresPlaceholdersRespectLiteralsAndBooleans(t *testing.T) {
	query := `SELECT '?', name FROM documents d WHERE d.is_folder = 0 AND note = 'is_folder = 1 ?' AND id = ?`
	got := postgresPlaceholders(query)
	want := `SELECT '?', name FROM documents d WHERE d.is_folder = FALSE AND note = 'is_folder = 1 ?' AND id = $1`
	if got != want {
		t.Fatalf("postgres query = %q, want %q", got, want)
	}
}

func TestPostgresPrefixQuery(t *testing.T) {
	if got := postgresPrefixQuery(`Zankyo - court filing`); got != "zankyo:* & court:* & filing:*" {
		t.Fatalf("prefix query = %q", got)
	}
	if got := postgresPrefixQuery("   "); got != "''" {
		t.Fatalf("empty prefix query = %q", got)
	}
}
