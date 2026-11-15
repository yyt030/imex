package list

import (
	"github.com/pkg/errors"

	"github.com/imex/internal/config"
)

// buildGaussQuery is the GaussDB / openGauss counterpart of buildPGQuery. Only
// one object needs its own statement: `indexes`. The Gauss kernel reports
// server_version 9.2.4 and does not parse the `CROSS JOIN LATERAL
// unnest(ix.indkey) WITH ORDINALITY` that PostgreSQL's buildPGIndexes uses.
// Every other object's PostgreSQL statement was measured on the live
// openGauss-lite 5.0.3 bed and passes unchanged, so it is delegated rather than
// copied — keeping a single source of truth for those eight queries.
func buildGaussQuery(cfg *config.ListConfig) (string, []any, []string, error) {
	if cfg.Object == ObjectIndexes {
		return buildGaussIndexes(cfg)
	}
	return buildPGQuery(cfg)
}

// buildGaussIndexes unwraps an index's key columns without LATERAL.
//
// generate_subscripts(ix.indkey, 1) is a set-returning function in the SELECT
// list, which a 9.2 kernel does parse (a FROM-clause reference to the same
// function is rejected with "invalid reference to FROM-clause entry"). The
// derived table s therefore produces one row per key column carrying both its
// ordinal and the int2vector; the outer join resolves the ordinal to the column
// name through pg_attribute. Measured with PREPARE/EXECUTE on openGauss-lite
// 5.0.3: one row for imex_dst_pkey, ord 0 -> SEQ_IN_INDEX 1, attname id.
//
// The scoping and placeholder order are inherited from the PostgreSQL builder:
// current_database() = $1, c.relname = $2, and the optional pattern is $3.
func buildGaussIndexes(cfg *config.ListConfig) (string, []any, []string, error) {
	if cfg.Database == "" {
		return "", nil, nil, errors.New("--database is required for list indexes")
	}
	if cfg.Table == "" {
		return "", nil, nil, errors.New("--table is required for list indexes")
	}
	b := &pgBuilder{}
	conds := pgScopedToNamespace("n.nspname", cfg, b)
	conds = append(conds, "c.relname = "+b.addArg(cfg.Table))
	conds = append(conds, pgPatternCondition("i.relname", cfg.Pattern, b)...)

	query := "SELECT s.nspname, s.relname, s.indexrelname, s.non_unique," +
		" s.ord + 1, a.attname, s.indnkeyatts, s.amname" +
		" FROM (" +
		" SELECT n.nspname, c.relname, i.relname AS indexrelname," +
		" CASE WHEN ix.indisunique THEN 0 ELSE 1 END AS non_unique," +
		" generate_subscripts(ix.indkey, 1) AS ord," +
		" ix.indkey AS indkey, c.oid AS relid," +
		" ix.indnkeyatts, am.amname" +
		" FROM pg_catalog.pg_index ix" +
		" JOIN pg_catalog.pg_class c ON c.oid = ix.indrelid" +
		" JOIN pg_catalog.pg_class i ON i.oid = ix.indexrelid" +
		" JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace" +
		" JOIN pg_catalog.pg_am am ON am.oid = i.relam" +
		pgWhereClause(conds) +
		" ) s" +
		" JOIN pg_catalog.pg_attribute a ON a.attrelid = s.relid" +
		" AND a.attnum = s.indkey[s.ord]" +
		" ORDER BY s.indexrelname, s.ord"
	return query, b.args, []string{
		"database", "table", "index", "non_unique", "seq", "column", "cardinality", "index_type",
	}, nil
}
