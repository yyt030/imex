package backend

import (
	"github.com/imex/internal/config"
)

// PostgreSQL's option refusals, one table per command, in the order `imex` has
// always reported them.
//
// The list is the port's honest statement of what it does not do rather than a
// translation layer: the alternative is to rewrite MySQL grammar into PostgreSQL's
// and ship statements nobody measured against a server, and a wrong ON CONFLICT
// target or a mis-shaded CREATE TABLE costs the user a table, not a message. An
// exit-2 refusal with the reason and the flag to drop is the honest version of the
// same run, and it keeps contract face ③ in the tool's own words instead of the
// server's.
//
// Each rule fires on the *value*, not on whether the option was named
// (--format sql, --threads 4, --mode create), so a run configured by any means
// cannot escape it. The two rules that fire on presence (--session-var) read the
// bool cmd folded in, exactly like the format-scoped options cmd/validate.go
// rejects, so a shipped default stays usable.
//
// What is left working is the point of the increment: a single-threaded DEL or
// Parquet dump of any SELECT, with the tool's own delimiters, escaping and NULL
// spelling, read through the same sql.RawBytes pipeline MySQL uses.

const pgSessionVarReason = "SET <name>=<value> is MySQL session grammar and imex does not translate it, put PostgreSQL settings in the server or role configuration"

func (postgresBackend) Refusals(c Command) []Refusal {
	switch c {
	case CmdExport:
		return []Refusal{
			{
				Option:  static("--format sql"),
				Reason:  "the SQL writer emits MySQL INSERT text (backquoted names and MySQL escaping) that no PostgreSQL server accepts, use --format del or --format parquet",
				Applies: func(o RunOptions) bool { return o.Format == "sql" },
			},
			{
				Option:  static("--threads"),
				Reason:  "parallel export shards the query with MySQL-only statements and has not been measured against PostgreSQL, use --threads 1",
				Applies: func(o RunOptions) bool { return o.Threads > 1 },
			},
			{
				Option:  static("--no-hex-blob"),
				Reason:  "a PostgreSQL server rejects a NUL inside any text value (SQLSTATE 22021), so bytea columns must go out as 0x hex; imex always does it on this engine",
				Applies: func(o RunOptions) bool { return o.NoHexBlob },
			},
			{
				Option:  static("--session-var"),
				Reason:  pgSessionVarReason,
				Applies: func(o RunOptions) bool { return o.SessionVar },
			},
		}
	case CmdImport:
		// Only --mode create is refused, and for a reason the mode switch cannot fix:
		// the DDL comes out of the MySQL dump reader, which this engine refuses on
		// the other side of the same chain (see CmdShow). --mode replace empties the
		// table with DELETE, which both engines parse, and the referential-check
		// toggle that MySQL wraps around it is the backend's to supply — PostgreSQL
		// has no such session setting and gets no statement rather than a failing
		// one. --mode insert_update builds an ON CONFLICT whose target the importer
		// reads out of the catalogue instead of guessing.
		return []Refusal{
			{
				Option:  static("--format sql"),
				Reason:  "a SQL dump is parsed with the MySQL statement grammar and carries MySQL INSERT text, import a --format del or --format parquet file instead",
				Applies: func(o RunOptions) bool { return o.Format == "sql" },
			},
			{
				Option:  static("--mode create"),
				Reason:  "it runs CREATE TABLE text out of a MySQL dump, which PostgreSQL does not accept, and imex cannot export that DDL on this engine either; create the table with pg_dump or your own DDL and use --mode insert",
				Applies: func(o RunOptions) bool { return o.Mode == string(config.ImportModeCreate) },
			},
			{
				Option:  static("--session-var"),
				Reason:  pgSessionVarReason,
				Applies: func(o RunOptions) bool { return o.SessionVar },
			},
		}
	case CmdList:
		// The other seven objects come from the PostgreSQL catalogue instead of
		// information_schema (internal/list/queries_pg.go), with the same
		// column names and the same --pattern and --all behaviour. Events are the one
		// object MySQL has that PostgreSQL does not have at all, so there is nothing
		// to report rather than an empty list that reads like a database without
		// schedules. The name is spelled here because internal/list imports this
		// package and cannot be imported back.
		return []Refusal{
			{
				Option:  static("list events"),
				Reason:  "PostgreSQL has no event scheduler, so there is nothing to list; scheduled work lives in cron or pgAgent",
				Applies: func(o RunOptions) bool { return o.Object == "events" },
			},
		}
	case CmdShow:
		// A definition is not a row: SHOW CREATE TABLE produces MySQL DDL, and the
		// PostgreSQL equivalents (pg_get_functiondef, pg_index, a view's stored
		// definition) would have to be re-imported by --mode create through the SQL
		// reader this engine also refuses. Until both halves exist, the honest answer
		// is that the tool does not do it, and pg_dump does. One rule covers every
		// kind, and it spells the kind into the message so a run says what it refused.
		return []Refusal{
			{
				Option:  func(o RunOptions) string { return "show " + o.ShowKind },
				Reason:  "definitions are read with SHOW CREATE, which PostgreSQL does not have, and imex cannot import the DDL it would produce; use pg_dump",
				Applies: func(o RunOptions) bool { return o.ShowKind != "" },
			},
		}
	}
	return nil
}

// ValidateCharset resolves --default-charset against this engine. MySQL sends
// SET NAMES with it; PostgreSQL takes it as the connection's client_encoding, which
// has fewer names available. validateCharset has already accepted the value by the
// time this runs, so what is reported here is a charset that exists but has no
// counterpart on this side.
func (postgresBackend) ValidateCharset(charset string) error {
	_, err := ClientEncoding(charset)
	return err
}
