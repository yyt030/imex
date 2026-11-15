package backend

import (
	"github.com/imex/internal/config"
)

// Dameng (DM8 Oracle mode) option refusals. What the engine serves on each
// command:
//   - export: --format del/sql/parquet, --threads > 1 included, any --query,
//     and --session-var (translated to ALTER SESSION SET, see below)
//   - import: --format del/sql/parquet, --mode insert/replace, --session-var
//   - list: databases, schemas, tables, views, columns, indexes, triggers and
//     routines (everything but events, which DM has no scheduler for)
//   - show: every kind but events
// Everything else is refused here so the run stops before it dials.
//
// --format sql round-trips both ways now. The writer emits a Dameng/Oracle
// grammar (double-quoted identifiers, HEXTORAW('..') binary and SQL-standard
// string literals), and the SQL reader is dialect-aware: on the Dameng grammar it
// decodes HEXTORAW('..') as bytes and treats a backslash in a string as a literal
// byte rather than an escape, so a Dameng `-f sql` dump imports back into Dameng.
// The reader is still a same-engine replay parser — it does not attempt to read an
// arbitrary external Oracle/dexp dump — so cross-engine transport stays DEL/parquet.
//
// --threads > 1 is no longer refused: the shard planner learned the Dameng
// catalogue (internal/export/shard_dialect.go) and sharded dumps were measured
// against the single-threaded one on a real DM8. del and sql came out byte-for-
// byte identical where the shard order matches (single-key tables, all threads
// values tried); with a composite key the ranges are expanded column by column
// (Dameng has no row-value comparison), which keeps the row multiset identical
// but not the byte order, so those are compared sorted. parquet never shards —
// like every other engine, the writer falls back to a single-threaded dump.
//
// --session-var is no longer refused either: DM8 does have session-level
// parameters, they are just spelled ALTER SESSION SET '<name>' = <value>
// rather than MySQL's SET. The translation lives in dameng_sessionvar.go,
// which decorates the driver's Connector so the statements run on every new
// physical connection. What is still refused is listed below.

func (damengBackend) Refusals(c Command) []Refusal {
	switch c {
	case CmdExport:
		return []Refusal{
			{
				Option:  static("--blob-encoding raw"),
				Reason:  "Dameng rejects a NUL byte inside a text value, so binary columns must go out as 0x hex; imex always does it on this engine",
				Applies: func(o RunOptions) bool { return o.NoHexBlob },
			},
		}
	case CmdImport:
		return []Refusal{
			{
				Option:  static("--mode insert_update"),
				Reason:  "the Dameng backend does not implement upsert in this MVP, use --mode insert or --mode replace",
				Applies: func(o RunOptions) bool { return o.Mode == string(config.ImportModeInsertUpdate) },
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
