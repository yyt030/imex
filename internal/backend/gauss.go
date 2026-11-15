package backend

import (
	"strings"

	"github.com/pkg/errors"

	"github.com/imex/internal/config"
)

// gaussBackend drives the GaussDB family (GaussDB / openGauss). It is a public
// engine as of round 185, measured end to end on openGauss-lite 5.0.3 (build
// 89d144c2, server_version 9.2.4).
//
// It speaks the PostgreSQL wire protocol and is reached through pgx exactly as
// postgresBackend is, so quoting, batching, snapshot, value classification and
// the import pipeline are inherited unchanged. What differs is that a Gauss
// server is a PostgreSQL-9.2-era kernel:
//
//   - pg_attribute has no attgenerated / attidentity columns;
//   - FROM-clause LATERAL and WITH ORDINALITY are not parsed;
//   - ON CONFLICT (upsert) does not exist;
//   - `show tab` is served by the native pg_get_tabledef(regclass), whose text
//     carries a `SET search_path` header, a WITH (orientation=...) clause and a
//     trailing semicolon of its own.
//
// Every deviation below is an override on this engine. Not one PostgreSQL,
// MySQL or Dameng statement is rewritten to accommodate Gauss: contract face ⑤
// for the three released engines stays byte-identical. The replacements were
// parsed by PREPARE and executed against the live openGauss bed before landing.
type gaussBackend struct {
	postgresBackend
}

func (gaussBackend) Engine() Engine { return EngineGauss }

// gaussShowColumn is the result column a Gauss pg_get_tabledef call returns:
// a scalar function call names its single output column after the function.
const gaussShowColumn = "pg_get_tabledef"

// ShowDefinition prints a table via the native Gauss pg_get_tabledef.
//
// The reference is passed as a SQL text literal to the function (it casts to
// regclass internally), not as a quoted identifier, so a single quote in a name
// is doubled — the standard literal escape — to keep the statement inert against
// a crafted object name. pg_get_tabledef's text already ends in `;` (measured on
// the live bed), so hasTerminator is true and show must not append a second one.
// The other kinds are refused by Refusals(CmdShow) and never reach here.
func (gaussBackend) ShowDefinition(object, schema, name string) (string, string, bool, bool) {
	if object != "table" {
		return "", "", false, false
	}
	ref := name
	if schema != "" {
		ref = schema + "." + name
	}
	ref = strings.ReplaceAll(ref, "'", "''")
	return "SELECT pg_get_tabledef('" + ref + "')", gaussShowColumn, true, true
}

// GeneratedColumnsSQL replaces the PostgreSQL pg_attribute probe. A 9.2-era
// catalogue has no attgenerated / attidentity columns, so the inherited query
// fails with SQLSTATE 42703 and the importer logs a WARN on every run. Reading
// information_schema instead is both parseable on this kernel and future-proof:
// a later GaussDB that does expose generated columns reports them here and the
// INSERT omits them as it should. Measured on openGauss-lite 5.0.3: the columns
// exist, ordinary ones read is_generated='NEVER' / is_identity='NO', and the
// query returns zero rows for a plain table.
func (gaussBackend) GeneratedColumnsSQL() string {
	return `SELECT UPPER(column_name) FROM information_schema.columns
WHERE table_catalog = $1 AND table_schema = current_schema() AND table_name = $2
  AND (is_generated <> 'NEVER' OR is_identity <> 'NO')`
}

// UniqueKeyIndexColumnsSQL replaces the PostgreSQL LATERAL/WITH ORDINALITY
// unwind with information_schema's constraint-to-column join. Both views exist
// on this kernel and key_column_usage.ordinal_position preserves the key column
// order the caller needs. The primary-key rows still sort first via the CASE.
func (gaussBackend) UniqueKeyIndexColumnsSQL() string {
	return `SELECT tc.constraint_name,
       CASE WHEN tc.constraint_type = 'PRIMARY KEY' THEN 1 ELSE 0 END,
       kcu.column_name
FROM information_schema.table_constraints tc
JOIN information_schema.key_column_usage kcu
  ON kcu.constraint_catalog = tc.constraint_catalog
 AND kcu.constraint_schema = tc.constraint_schema
 AND kcu.constraint_name = tc.constraint_name
WHERE tc.table_catalog = $1 AND tc.table_schema = current_schema()
  AND tc.table_name = $2
  AND tc.constraint_type IN ('PRIMARY KEY','UNIQUE')
ORDER BY (tc.constraint_type = 'PRIMARY KEY') DESC,
         tc.constraint_name, kcu.ordinal_position`
}

// UpsertSuffix refuses to build the inherited ON CONFLICT clause. The Gauss
// kernel rejects `ON CONFLICT (...)` with a bare syntax error; the CLI refuses
// --mode insert_update before dialling (see Refusals), and this keeps a library
// caller that bypasses the CLI from emitting a statement the server cannot
// parse either.
func (gaussBackend) UpsertSuffix(string, []string, []string) (string, string, error) {
	return "", "", errors.New("upsert is not supported on the GaussDB / openGauss engine: the server has no ON CONFLICT; use --mode insert or --mode replace")
}

func (gaussBackend) UpsertNeedsTarget() bool { return false }

// Refusals states, in reporting order, what this engine cannot serve. The three
// commands other than `show` used to be inherited verbatim from PostgreSQL,
// which made the reasons claim "a PostgreSQL server" and "PostgreSQL has no
// event scheduler" on a --engine gauss run. Those are now the engine's own
// words. `show` keeps the pg_get_tabledef whitelist (table/tab) and names
// pg_dump as the alternative for the other kinds.
//
// Note the kind arrives as the CLI's SHORT name (cmd folds kind.Short into
// RunOptions.ShowKind), while show.Run later looks the definition up by the FULL
// word. Both spellings of the table kind are whitelisted so `show tab` and
// `show table` reach pg_get_tabledef; every other kind, and empty, is refused.
func (g gaussBackend) Refusals(c Command) []Refusal {
	switch c {
	case CmdExport:
		return []Refusal{
			{
				Option:  static("--blob-encoding raw"),
				Reason:  "a GaussDB / openGauss server rejects a NUL inside any text value, so bytea columns must go out as 0x hex; imex always does it on this engine",
				Applies: func(o RunOptions) bool { return o.NoHexBlob },
			},
		}
	case CmdImport:
		// Nothing is refused except the mode whose SQL this kernel cannot parse.
		return []Refusal{
			{
				Option:  static("--mode insert_update"),
				Reason:  "the GaussDB / openGauss server has no ON CONFLICT, so an upsert cannot be built; use --mode insert or --mode replace",
				Applies: func(o RunOptions) bool { return o.Mode == string(config.ImportModeInsertUpdate) },
			},
		}
	case CmdList:
		return []Refusal{
			{
				Option:  static("list events"),
				Reason:  "GaussDB / openGauss has no event scheduler, so there is nothing to list; scheduled work lives in an external scheduler",
				Applies: func(o RunOptions) bool { return o.Object == "events" },
			},
		}
	case CmdShow:
		return []Refusal{
			{
				Option: func(o RunOptions) string { return "show " + o.ShowKind },
				Reason: "Gauss table definitions are read with the native pg_get_tabledef; the other kinds have no equivalent definition function on this engine, use pg_dump for those",
				Applies: func(o RunOptions) bool {
					return o.ShowKind != "" && o.ShowKind != "table" && o.ShowKind != "tab"
				},
			},
		}
	}
	return nil
}

// ValidateCharset applies the same client_encoding map as PostgreSQL but names
// this engine in the error. The inherited message said "--engine pg" because
// ClientEncoding hard-codes it; on a --engine gauss run that was simply wrong.
func (gaussBackend) ValidateCharset(charset string) error {
	if _, err := ClientEncoding(charset); err != nil {
		return errors.Errorf("invalid --default-charset %q with --engine gauss: GaussDB / openGauss has no such client encoding (supported: utf8mb4, latin1, gbk, gb18030, ascii)", charset)
	}
	return nil
}
