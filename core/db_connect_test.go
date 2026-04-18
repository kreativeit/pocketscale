package core

import (
	"os"
	"strings"
	"testing"
)

func TestSchemaFromPath(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		in   string
		want string
	}{
		{"", DataSchemaName},
		{".", DataSchemaName},
		{"data.db", DataSchemaName},
		{"/var/pb_data/data.db", DataSchemaName},
		{"auxiliary.db", AuxSchemaName},
		{"/var/pb_data/auxiliary.db", AuxSchemaName},
		{"custom.db", "custom"},
		{"/tmp/My-Custom.DB", "my_custom"},
		{"/tmp/1starts-with-digit.db", "pb_1starts_with_digit"},
	}

	for _, s := range scenarios {
		if got := schemaFromPath(s.in); got != s.want {
			t.Errorf("schemaFromPath(%q): want %q, got %q", s.in, s.want, got)
		}
	}
}

func TestSanitizeSchemaName(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		in   string
		want string
	}{
		{"", "pb_"},
		{"valid", "valid"},
		{"Mixed_Case", "mixed_case"},
		{"with spaces", "with_spaces"},
		{"Weird!@#Name", "weird___name"},
		{"123leading", "pb_123leading"},
		{"!!!", "__"},
	}

	for _, s := range scenarios {
		if got := sanitizeSchemaName(s.in); got != s.want {
			t.Errorf("sanitizeSchemaName(%q): want %q, got %q", s.in, s.want, got)
		}
	}
}

func TestDSNWithSearchPathURL(t *testing.T) {
	t.Parallel()

	got, err := dsnWithSearchPath("postgres://user:pass@host:5432/db?sslmode=disable", "pb_data")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// query parameter order is non-deterministic; assert substrings
	if !strings.Contains(got, "search_path=pb_data%2Cpublic") {
		t.Errorf("expected search_path to be url-encoded in %q", got)
	}
	if !strings.HasPrefix(got, "postgres://user:pass@host:5432/db?") {
		t.Errorf("expected URL prefix preserved, got %q", got)
	}
	if !strings.Contains(got, "sslmode=disable") {
		t.Errorf("expected existing query params preserved, got %q", got)
	}
}

func TestDSNWithSearchPathKV(t *testing.T) {
	t.Parallel()

	got, err := dsnWithSearchPath("host=localhost user=postgres dbname=postgres", "pb_aux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(got, "options='-c search_path=pb_aux,public'") {
		t.Errorf("expected options clause, got %q", got)
	}
}

func TestQuotePGIdentifier(t *testing.T) {
	t.Parallel()

	if got := quotePGIdentifier(`simple`); got != `"simple"` {
		t.Errorf("want %q, got %q", `"simple"`, got)
	}
	if got := quotePGIdentifier(`with"quote`); got != `"with""quote"` {
		t.Errorf("want %q, got %q", `"with""quote"`, got)
	}
}

// TestDefaultDBConnect_Live verifies that DefaultDBConnect can establish
// a working connection to a real Postgres instance, auto-create the
// target schema, and execute a trivial round-trip query.
//
// The test is skipped unless the PB_TEST_PG_DSN environment variable is
// set to a reachable Postgres DSN (typically pointing at an ephemeral
// container spun up by the CI harness or local developer script).
func TestDefaultDBConnect_Live(t *testing.T) {
	dsn := os.Getenv("PB_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("PB_TEST_PG_DSN not set; skipping live Postgres smoke test")
	}

	t.Setenv(DefaultDBDSNEnv, dsn)

	// data schema
	dataDB, err := DefaultDBConnect("/tmp/pb/data.db")
	if err != nil {
		t.Fatalf("DefaultDBConnect(data.db) failed: %v", err)
	}
	defer dataDB.Close()

	var one int
	if err := dataDB.NewQuery("SELECT 1").Row(&one); err != nil {
		t.Fatalf("round-trip SELECT 1 failed: %v", err)
	}
	if one != 1 {
		t.Fatalf("want 1, got %d", one)
	}

	var schema string
	if err := dataDB.NewQuery("SELECT current_schema()").Row(&schema); err != nil {
		t.Fatalf("current_schema failed: %v", err)
	}
	if schema != DataSchemaName {
		t.Errorf("expected current_schema() %q, got %q", DataSchemaName, schema)
	}

	// aux schema
	auxDB, err := DefaultDBConnect("/tmp/pb/auxiliary.db")
	if err != nil {
		t.Fatalf("DefaultDBConnect(auxiliary.db) failed: %v", err)
	}
	defer auxDB.Close()

	if err := auxDB.NewQuery("SELECT current_schema()").Row(&schema); err != nil {
		t.Fatalf("aux current_schema failed: %v", err)
	}
	if schema != AuxSchemaName {
		t.Errorf("expected aux current_schema() %q, got %q", AuxSchemaName, schema)
	}
}
