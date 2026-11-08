package backend

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
// (--threads above 1, --no-hex-blob switched on), so a run configured by any
// means cannot escape it. --session-var is no longer a refusal at all: this
// engine now honours it the way MySQL does, running a SET per new connection and
// rejecting only the names the tool or the driver owns
// (postgresBackend.IsReservedConnParam).
//
// What is left working is the point of the increment: a single-threaded DEL or
// Parquet dump of any SELECT, with the tool's own delimiters, escaping and NULL
// spelling, read through the same sql.RawBytes pipeline MySQL uses.

func (postgresBackend) Refusals(c Command) []Refusal {
	switch c {
	case CmdExport:
		return []Refusal{
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
		}
	case CmdImport:
		// Nothing is refused on import. --session-var is honoured like MySQL's;
		// --mode create no longer exists on any engine (imex moves rows into an
		// existing table and replays no DDL, so schema creation is left to pg_dump
		// or your own migrations); --format sql is read by the dialect-blind reader
		// and re-bound through this engine's own INSERT builder rather than replayed;
		// --mode replace empties the table with DELETE, which both engines parse, and
		// the referential-check toggle MySQL wraps around it is the backend's to
		// supply — PostgreSQL has no such session setting and gets no statement
		// rather than a failing one; --mode insert_update builds an ON CONFLICT whose
		// target the importer reads out of the catalogue instead of guessing.
		return nil
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
		// Definitions are printable: table/view/trigger/procedure/function are
		// rebuilt from the catalog (see show_postgres.go), so only `event` is
		// refused — PostgreSQL has no event scheduler, so there is no object of
		// that kind to print, exactly as `list events` is refused above. The kind
		// arrives as the CLI's short name.
		return []Refusal{
			{
				Option:  func(o RunOptions) string { return "show " + o.ShowKind },
				Reason:  "PostgreSQL has no event scheduler, so there is no event definition to show; scheduled work lives in cron or pgAgent",
				Applies: func(o RunOptions) bool { return o.ShowKind == "evt" || o.ShowKind == "event" },
			},
		}
	}
	return nil
}

// ValidateCharset resolves --default-charset against this engine. MySQL sends
// SET NAMES with it; PostgreSQL takes it as the connection's client_encoding, which
// has fewer names available. This check runs before cmd's format rules, so the
// engine reports a name it cannot honour even when that name is in MySQL's list.
func (postgresBackend) ValidateCharset(charset string) error {
	_, err := ClientEncoding(charset)
	return err
}
