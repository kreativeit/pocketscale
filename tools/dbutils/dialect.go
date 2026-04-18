package dbutils

import (
	"fmt"
	"strings"
)

// Dialect defines a minimal database-specific SQL generator used by PocketBase
// to emit portions of raw SQL (JSON helpers, conditional expressions, LIKE
// operator, etc.) that differ between database engines.
//
// This is the first step in decoupling PocketBase's SQL generation from
// SQLite-specific syntax. Only a subset of the surface area is covered for
// now (the JSON/IIF/LIKE helpers that are used from multiple places in the
// codebase); other SQLite-specific areas such as dynamic collection-table
// DDL, schema introspection via PRAGMA / sqlite_master, view creation and
// online backups are NOT yet dialect-aware and continue to assume SQLite.
type Dialect interface {
	// Name returns a short identifier for the dialect (e.g. "sqlite",
	// "postgres"). It is primarily intended for logging and switching
	// logic in callers that cannot be fully expressed through the
	// interface yet.
	Name() string

	// JSONExtract returns an SQL expression that extracts the value at
	// the given JSON path from the provided column. The generated
	// expression gracefully degrades for non-JSON column values.
	JSONExtract(column string, path string) string

	// JSONEach returns an SQL table-valued expression that iterates over
	// the elements of the JSON array stored in the given column. Non-array
	// and non-JSON values are wrapped into a single-element array so that
	// iteration still yields one row.
	JSONEach(column string) string

	// JSONArrayLength returns an SQL expression that evaluates to the
	// length of the JSON array stored in the given column. Non-array and
	// non-JSON values are treated as a single-element array; empty or
	// NULL values evaluate to 0.
	JSONArrayLength(column string) string

	// IIF returns an SQL expression equivalent to
	// `CASE WHEN <condition> THEN <trueExpr> ELSE <falseExpr> END`.
	IIF(condition, trueExpr, falseExpr string) string

	// LikeOperator returns the operator keyword used for a case-insensitive
	// substring match (`LIKE` on SQLite which is case-insensitive by
	// default, `ILIKE` on Postgres).
	LikeOperator() string

	// NotLikeOperator returns the negated form of [Dialect.LikeOperator].
	NotLikeOperator() string
}

// --- SQLite ------------------------------------------------------------

// SQLiteDialect implements [Dialect] using SQLite syntax. Its output is
// byte-for-byte identical to the pre-existing helpers in this package so
// that the introduction of the dialect abstraction is backward compatible.
type SQLiteDialect struct{}

// Name implements [Dialect.Name].
func (SQLiteDialect) Name() string { return "sqlite" }

// JSONExtract implements [Dialect.JSONExtract].
func (SQLiteDialect) JSONExtract(column string, path string) string {
	if path != "" && !strings.HasPrefix(path, "[") {
		path = "." + path
	}

	return fmt.Sprintf(
		// note: the extra object wrapping is needed to workaround the cases where a json_extract is used with non-json columns.
		"(CASE WHEN json_valid([[%s]]) THEN JSON_EXTRACT([[%s]], '$%s') ELSE JSON_EXTRACT(json_object('pb', [[%s]]), '$.pb%s') END)",
		column,
		column,
		path,
		column,
		path,
	)
}

// JSONEach implements [Dialect.JSONEach].
func (SQLiteDialect) JSONEach(column string) string {
	// note: we are not using the new and shorter "if(x,y)" syntax for
	// compatibility with custom drivers that use older SQLite version
	return fmt.Sprintf(
		`json_each(CASE WHEN iif(json_valid([[%s]]), json_type([[%s]])='array', FALSE) THEN [[%s]] ELSE json_array([[%s]]) END)`,
		column, column, column, column,
	)
}

// JSONArrayLength implements [Dialect.JSONArrayLength].
func (SQLiteDialect) JSONArrayLength(column string) string {
	// note: we are not using the new and shorter "if(x,y)" syntax for
	// compatibility with custom drivers that use older SQLite version
	return fmt.Sprintf(
		`json_array_length(CASE WHEN iif(json_valid([[%s]]), json_type([[%s]])='array', FALSE) THEN [[%s]] ELSE (CASE WHEN [[%s]] = '' OR [[%s]] IS NULL THEN json_array() ELSE json_array([[%s]]) END) END)`,
		column, column, column, column, column, column,
	)
}

// IIF implements [Dialect.IIF]. It uses SQLite's native iif() builtin to
// preserve compatibility with pre-existing generated SQL.
func (SQLiteDialect) IIF(condition, trueExpr, falseExpr string) string {
	return fmt.Sprintf("iif(%s, %s, %s)", condition, trueExpr, falseExpr)
}

// LikeOperator implements [Dialect.LikeOperator]. SQLite's LIKE is
// case-insensitive by default for ASCII characters.
func (SQLiteDialect) LikeOperator() string { return "LIKE" }

// NotLikeOperator implements [Dialect.NotLikeOperator].
func (SQLiteDialect) NotLikeOperator() string { return "NOT LIKE" }

// --- Postgres ----------------------------------------------------------

// PostgresDialect implements [Dialect] using PostgreSQL syntax.
//
// The JSON helpers cast the column to jsonb on the fly so that they can
// operate on text-typed columns (PocketBase stores JSON values in text
// columns). Values that do not parse as JSON are wrapped into a
// single-element array, mirroring the SQLite behavior.
//
// Note: Postgres is not yet a fully supported target - many other
// SQLite-specific code paths (DDL, PRAGMA-based introspection, online
// backups, views) still need to be ported. See the package documentation
// for details.
type PostgresDialect struct{}

// Name implements [Dialect.Name].
func (PostgresDialect) Name() string { return "postgres" }

// pgJSONBArrayExpr returns a Postgres SQL fragment that evaluates the
// column as a jsonb array: if the column already contains a JSON array it
// is returned as jsonb; otherwise the raw text value is wrapped into a
// single-element jsonb array so that iteration/length semantics mirror
// [SQLiteDialect].
//
// NULL and empty-string columns evaluate to an empty jsonb array.
//
// The helper assumes the column is of a text type; PocketBase stores all
// of its JSON values in text columns. Callers must feed a column
// reference (e.g. "[[my_col]]"), not an arbitrary expression, to keep the
// fragment safe to embed.
func pgJSONBArrayExpr(column string) string {
	return fmt.Sprintf(
		"(CASE "+
			"WHEN [[%s]] IS NULL OR [[%s]] = '' THEN '[]'::jsonb "+
			"WHEN jsonb_typeof([[%s]]::jsonb) = 'array' THEN [[%s]]::jsonb "+
			"ELSE jsonb_build_array([[%s]]) END)",
		column, column, column, column, column,
	)
}

// JSONExtract implements [Dialect.JSONExtract].
//
// The Postgres implementation uses the `#>` operator with a text-array
// path built from the provided dotted/bracketed path string. When the
// column does not contain valid JSON the raw text value is returned as-is.
func (PostgresDialect) JSONExtract(column string, path string) string {
	pgPath := jsonPathToPgArray(path)

	return fmt.Sprintf(
		"(CASE "+
			"WHEN [[%s]] IS NULL OR [[%s]] = '' THEN NULL "+
			"WHEN jsonb_typeof([[%s]]::jsonb) IS NOT NULL THEN [[%s]]::jsonb #> %s "+
			"ELSE to_jsonb([[%s]]) END)",
		column, column, column, column, pgPath, column,
	)
}

// JSONEach implements [Dialect.JSONEach].
func (PostgresDialect) JSONEach(column string) string {
	return fmt.Sprintf("jsonb_array_elements(%s)", pgJSONBArrayExpr(column))
}

// JSONArrayLength implements [Dialect.JSONArrayLength].
func (PostgresDialect) JSONArrayLength(column string) string {
	return fmt.Sprintf("jsonb_array_length(%s)", pgJSONBArrayExpr(column))
}

// IIF implements [Dialect.IIF].
func (PostgresDialect) IIF(condition, trueExpr, falseExpr string) string {
	return fmt.Sprintf("(CASE WHEN %s THEN %s ELSE %s END)", condition, trueExpr, falseExpr)
}

// LikeOperator implements [Dialect.LikeOperator].
// Postgres LIKE is case-sensitive; use ILIKE to match SQLite semantics.
func (PostgresDialect) LikeOperator() string { return "ILIKE" }

// NotLikeOperator implements [Dialect.NotLikeOperator].
func (PostgresDialect) NotLikeOperator() string { return "NOT ILIKE" }

// jsonPathToPgArray converts a JSON path expression of the form
// `a.b[0].c` into a Postgres text-array literal suitable for use with
// the `#>` operator, e.g. `'{a,b,0,c}'::text[]`.
//
// An empty path yields `'{}'::text[]` which, when used with `#>`, returns
// the whole document.
func jsonPathToPgArray(path string) string {
	parts := splitJSONPath(path)
	if len(parts) == 0 {
		return "'{}'::text[]"
	}

	var b strings.Builder
	b.WriteString("'{")
	for i, p := range parts {
		if i > 0 {
			b.WriteByte(',')
		}
		// quote-escape for a Postgres array literal
		b.WriteString(strings.ReplaceAll(p, `"`, `""`))
	}
	b.WriteString("}'::text[]")
	return b.String()
}

// pgPathSuffix returns a comma-prefixed Postgres array-literal fragment
// for use inside the non-JSON fallback branch of [PostgresDialect.JSONExtract].
// It intentionally does not include the opening brace so that the caller
// can prepend a fixed "pb" path prefix.
func pgPathSuffix(path string) string {
	parts := splitJSONPath(path)
	if len(parts) == 0 {
		return "||'}'"
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteByte(',')
		b.WriteString(strings.ReplaceAll(p, `"`, `""`))
	}
	b.WriteString("}'")
	return "||'" + b.String()
}

// splitJSONPath splits a dotted/bracketed JSON path like `a.b[0].c` into
// its individual components: ["a", "b", "0", "c"]. Leading dots are
// ignored.
func splitJSONPath(path string) []string {
	if path == "" {
		return nil
	}

	// normalize `[N]` into `.N`
	normalized := strings.ReplaceAll(path, "[", ".")
	normalized = strings.ReplaceAll(normalized, "]", "")
	normalized = strings.TrimPrefix(normalized, ".")
	if normalized == "" {
		return nil
	}

	raw := strings.Split(normalized, ".")
	parts := make([]string, 0, len(raw))
	for _, p := range raw {
		if p == "" {
			continue
		}
		parts = append(parts, p)
	}
	return parts
}

// --- Default / package-level helpers ----------------------------------

// defaultDialect is the [Dialect] implementation used by the package-level
// helpers ([JSONExtract], [JSONEach], [JSONArrayLength]) so that existing
// callers continue to receive SQLite-flavored SQL without any changes.
var defaultDialect Dialect = SQLiteDialect{}
