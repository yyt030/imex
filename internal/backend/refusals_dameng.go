package backend

import (
	"github.com/imex/internal/config"
)

// Dameng (DM8 Oracle mode) option refusals. The first cut only supports:
//   - export: --format del/parquet, single-threaded, any --query
//   - import: --format del/parquet, --mode insert/replace
//   - list: databases, tables, columns, indexes
// Everything else is refused here so the run stops before it dials.

const dmSessionVarReason = "SET <name>=<value> is MySQL session grammar and imex does not translate it for Dameng; put session settings in the server or role configuration"

func (damengBackend) Refusals(c Command) []Refusal {
	switch c {
	case CmdExport:
		return []Refusal{
			{
				Option:  static("--format sql"),
				Reason:  "the SQL writer emits MySQL INSERT text (backquoted names and MySQL escaping) that no Dameng server accepts, use --format del or --format parquet",
				Applies: func(o RunOptions) bool { return o.Format == "sql" },
			},
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
				Reason:  "a SQL dump is parsed with the MySQL statement grammar and carries MySQL INSERT text, import a --format del or --format parquet file instead",
				Applies: func(o RunOptions) bool { return o.Format == "sql" },
			},
			{
				Option:  static("--mode insert_update"),
				Reason:  "the Dameng backend does not implement upsert in this MVP, use --mode insert or --mode replace",
				Applies: func(o RunOptions) bool { return o.Mode == string(config.ImportModeInsertUpdate) },
			},
			{
				Option:  static("--mode create"),
				Reason:  "it runs CREATE TABLE text out of a MySQL dump, which Dameng does not accept, and imex cannot export that DDL on this engine either; create the table with your own DDL and use --mode insert",
				Applies: func(o RunOptions) bool { return o.Mode == string(config.ImportModeCreate) },
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
				Reason: "this object has not been measured against Dameng Oracle-mode catalog; supported objects are databases, tables, columns and indexes",
				Applies: func(o RunOptions) bool {
					switch o.Object {
					case "databases", "tables", "columns", "indexes":
						return false
					}
					return true
				},
			},
		}
	case CmdShow:
		return []Refusal{
			{
				Option:  func(o RunOptions) string { return "show " + o.ShowKind },
				Reason:  "definitions are read with SHOW CREATE, which Dameng Oracle mode does not provide through the same catalog, and imex cannot import the DDL it would produce; use your own DDL",
				Applies: func(o RunOptions) bool { return o.ShowKind != "" },
			},
		}
	}
	return nil
}
