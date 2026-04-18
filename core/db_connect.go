package core

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" sql driver
	"github.com/pocketbase/dbx"
)

// DefaultDBDSNEnv is the name of the environment variable used to
// discover the default Postgres connection string when a DBConnect
// callback is not explicitly provided.
const DefaultDBDSNEnv = "PB_DB_DSN"

// DataSchemaName is the Postgres schema that holds the main application
// tables (collections, records, system tables). It is the Postgres
// equivalent of the legacy SQLite "data.db" file.
const DataSchemaName = "pb_data"

// AuxSchemaName is the Postgres schema that holds auxiliary tables
// (logs, bootstrap state, etc.). It is the Postgres equivalent of the
// legacy SQLite "auxiliary.db" file.
const AuxSchemaName = "pb_aux"

// defaultLocalDSN is the development-time fallback DSN used when
// [DefaultDBDSNEnv] is not set. Production deployments MUST set the env
// variable explicitly.
const defaultLocalDSN = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"

// DefaultDBConnect opens a new *dbx.DB connection backed by PostgreSQL.
//
// The legacy `dbPath` argument is retained for backward compatibility
// with the DBConnectFunc signature: its file basename is used to decide
// which logical schema the returned connection should operate on.
//
//   - basename "data.db"       → schema [DataSchemaName]
//   - basename "auxiliary.db"  → schema [AuxSchemaName]
//   - any other basename       → sanitized to a safe schema identifier
//
// The underlying DSN is taken from the PB_DB_DSN environment variable
// (see [DefaultDBDSNEnv]). If that variable is unset, a local
// development default is used.
//
// The function ensures that the target schema exists (CREATE SCHEMA IF
// NOT EXISTS) using a short-lived administrative connection before
// returning the application-facing *dbx.DB handle.
func DefaultDBConnect(dbPath string) (*dbx.DB, error) {
	schema := schemaFromPath(dbPath)

	baseDSN := os.Getenv(DefaultDBDSNEnv)
	if baseDSN == "" {
		baseDSN = defaultLocalDSN
	}

	if err := ensureSchemaExists(baseDSN, schema); err != nil {
		return nil, fmt.Errorf("failed to ensure schema %q: %w", schema, err)
	}

	dsn, err := dsnWithSearchPath(baseDSN, schema)
	if err != nil {
		return nil, err
	}

	return dbx.Open("pgx", dsn)
}

// schemaFromPath maps a legacy SQLite file path to a Postgres schema
// name. Only the file basename is considered so that callers in
// core.BaseApp can keep passing `<DataDir>/data.db` and
// `<DataDir>/auxiliary.db` unchanged.
func schemaFromPath(dbPath string) string {
	base := filepath.Base(dbPath)
	switch base {
	case "", ".", "data.db":
		return DataSchemaName
	case "auxiliary.db":
		return AuxSchemaName
	}

	return sanitizeSchemaName(strings.TrimSuffix(base, filepath.Ext(base)))
}

// sanitizeSchemaName converts an arbitrary string to a safe Postgres
// identifier suitable for use as an unquoted schema name.
func sanitizeSchemaName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case i > 0:
			b.WriteRune('_')
		}
	}

	result := b.String()
	if result == "" || (result[0] >= '0' && result[0] <= '9') {
		result = "pb_" + result
	}
	return result
}

// ensureSchemaExists opens a short-lived administrative connection
// (without any search_path overrides) and runs CREATE SCHEMA IF NOT
// EXISTS.
func ensureSchemaExists(baseDSN, schema string) error {
	db, err := sql.Open("pgx", baseDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", quotePGIdentifier(schema)))
	return err
}

// dsnWithSearchPath returns a copy of baseDSN with its `search_path`
// runtime parameter set so the given schema is searched first, followed
// by `public` for access to extensions (pgcrypto, uuid-ossp, etc.).
//
// Supports both URL-style ("postgres://...") and libpq keyword/value
// ("host=... dbname=...") connection strings.
func dsnWithSearchPath(baseDSN, schema string) (string, error) {
	searchPath := schema + ",public"

	if !strings.HasPrefix(baseDSN, "postgres://") && !strings.HasPrefix(baseDSN, "postgresql://") {
		cleaned := stripKVOption(baseDSN, "options")
		cleaned = stripKVOption(cleaned, "search_path")
		return strings.TrimSpace(cleaned) + fmt.Sprintf(" options='-c search_path=%s'", searchPath), nil
	}

	u, err := url.Parse(baseDSN)
	if err != nil {
		return "", fmt.Errorf("invalid DSN: %w", err)
	}
	q := u.Query()
	q.Set("search_path", searchPath)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// stripKVOption removes a key=value pair from a libpq keyword/value
// style connection string. Quoted values are not supported and should
// not appear for the keys we handle (`options`, `search_path`).
func stripKVOption(dsn, key string) string {
	fields := strings.Fields(dsn)
	out := make([]string, 0, len(fields))
	prefix := key + "="
	for _, f := range fields {
		if strings.HasPrefix(f, prefix) {
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

// quotePGIdentifier quotes a Postgres identifier by wrapping it in
// double quotes and escaping any contained double quote characters.
func quotePGIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
