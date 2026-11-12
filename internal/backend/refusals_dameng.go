package backend

import (
	"github.com/imex/internal/config"
)

// Dameng (DM8 Oracle mode) option refusals. The first cut only supports:
//   - export: --format del/parquet, single-threaded, any --query
//   - import: --format del/parquet, --mode insert/replace
//   - list: databases, schemas, tables, columns, indexes
// Everything else is refused here so the run stops before it dials.
//
// --format sql export is now supported (the writer gained a Dameng/Oracle
// grammar: double-quoted identifiers, HEXTORAW('..') binary and SQL-standard
// string literals, all pinned against live DM8 disql). --format sql import is
// still refused because the SQL reader still parses with the MySQL statement
// grammar and cannot yet replay a Dameng dump.

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
				Option:  static("--format sql"),
				Reason:  "imex can now export a Dameng-dialect --format sql dump, but the SQL reader still parses with the MySQL statement grammar and cannot replay a Dameng dump; import a --format del or --format parquet file, or load the dump with your own DM client",
				Applies: func(o RunOptions) bool { return o.Format == "sql" },
			},
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
		return []Refusal{
			{
				Option: func(o RunOptions) string { return "list " + o.Object },
				Reason: "this object has not been measured against Dameng Oracle-mode catalog; supported objects are databases, schemas, tables, columns and indexes",
				Applies: func(o RunOptions) bool {
					switch o.Object {
					case "databases", "schemas", "tables", "columns", "indexes":
						return false
					}
					return true
				},
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
