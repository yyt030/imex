package backend

import (
	"github.com/imex/internal/config"
)

// Dameng (DM8 Oracle mode) option refusals. The first cut only supports:
//   - export: --format del/sql/parquet, single-threaded, any --query
//   - import: --format del/sql/parquet, --mode insert/replace
//   - list: databases, schemas, tables, views, columns, indexes, triggers and
//     routines (everything but events, which DM has no scheduler for)
// Everything else is refused here so the run stops before it dials.
//
// --format sql round-trips both ways now. The writer emits a Dameng/Oracle
// grammar (double-quoted identifiers, HEXTORAW('..') binary and SQL-standard
// string literals), and the SQL reader is dialect-aware: on the Dameng grammar it
// decodes HEXTORAW('..') as bytes and treats a backslash in a string as a literal
// byte rather than an escape, so a Dameng `-f sql` dump imports back into Dameng.
// The reader is still a same-engine replay parser — it does not attempt to read an
// arbitrary external Oracle/dexp dump — so cross-engine transport stays DEL/parquet.

const dmSessionVarReason = "SET <name>=<value> is MySQL session grammar and imex does not translate it for Dameng; put session settings in the server or role configuration"

func (damengBackend) Refusals(c Command) []Refusal {
	switch c {
	case CmdExport:
		return []Refusal{
			{
				Option:  static("--threads"),
				Reason:  "parallel export has not been measured against Dameng, use --threads 1",
				Applies: func(o RunOptions) bool { return o.Threads > 1 },
			},
			{
				Option:  static("--no-hex-blob"),
				Reason:  "Dameng rejects a NUL byte inside a text value, so binary columns must go out as 0x hex; imex always does it on this engine",
				Applies: func(o RunOptions) bool { return o.NoHexBlob },
			},
			{
				Option:  static("--session-var"),
				Reason:  dmSessionVarReason,
				Applies: func(o RunOptions) bool { return o.SessionVar },
			},
		}
	case CmdImport:
		return []Refusal{
			{
				Option:  static("--mode insert_update"),
				Reason:  "the Dameng backend does not implement upsert in this MVP, use --mode insert or --mode replace",
				Applies: func(o RunOptions) bool { return o.Mode == string(config.ImportModeInsertUpdate) },
			},
			{
				Option:  static("--session-var"),
				Reason:  dmSessionVarReason,
				Applies: func(o RunOptions) bool { return o.SessionVar },
			},
		}
	case CmdList:
		// Every catalog object is served except events: DM8 Oracle mode has no
		// event scheduler, so there is nothing to list. The reason mirrors the
		// `show evt` refusal below, which reads the same absence.
		return []Refusal{
			{
				Option:  func(o RunOptions) string { return "list " + o.Object },
				Reason:  "Dameng Oracle mode has no event scheduler, so there is no scheduled event to list; scheduled work lives in the DM job package",
				Applies: func(o RunOptions) bool { return o.Object == "events" },
			},
		}
	case CmdShow:
		// Definitions are printable through DBMS_METADATA.GET_DDL (see
		// show_dameng.go), so only `event` is refused: DM8 Oracle mode has no
		// MySQL-style event scheduler, so there is no event object to show. The
		// kind arrives as the CLI's short name.
		return []Refusal{
			{
				Option:  func(o RunOptions) string { return "show " + o.ShowKind },
				Reason:  "Dameng Oracle mode has no event scheduler, so there is no event definition to show; scheduled work lives in the DM job package",
				Applies: func(o RunOptions) bool { return o.ShowKind == "evt" || o.ShowKind == "event" },
			},
		}
	}
	return nil
}
