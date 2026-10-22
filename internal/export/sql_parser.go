package export

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"

	"vitess.io/vitess/go/vt/sqlparser"
)

// splitLeadingOptimizerHint extracts a leading optimizer hint block
// (e.g. OceanBase/Oracle style "/*+ parallel(8) */" placed before the SELECT
// keyword). vitess parses such statements but silently drops this leading hint
// when re-serializing, so callers must preserve it separately. The returned
// `hint` is "" when the query does not start with an optimizer hint; `rest` is
// the query with the hint removed.
func splitLeadingOptimizerHint(query string) (hint, rest string) {
	trimmed := strings.TrimLeft(query, " \t\n\r")
	offset := len(query) - len(trimmed)
	if strings.HasPrefix(trimmed, "/*+") {
		if end := strings.Index(trimmed, "*/"); end != -1 {
			hintEnd := offset + end + 2
			return query[offset:hintEnd], query[hintEnd:]
		}
	}
	return "", query
}

// SQLParser handles parsing and modification of SQL queries for sharding
type SQLParser struct {
	parser *sqlparser.Parser
}

// NewSQLParser creates a new SQL parser instance
func NewSQLParser() *SQLParser {
	parser, err := sqlparser.New(sqlparser.Options{
		MySQLServerVersion: "8.0.30",
	})
	if err != nil {
		panic(fmt.Sprintf("failed to create SQL parser: %v", err))
	}
	return &SQLParser{
		parser: parser,
	}
}

// BuildShardedQuery takes an original query and injects shard conditions intelligently
func (p *SQLParser) BuildShardedQuery(originalQuery string, shardInfo *ShardInfo, shardRange ShardRange) (string, error) {
	// Preserve a leading optimizer hint (OceanBase/Oracle style "/*+ ... */"
	// placed before the SELECT keyword); vitess drops it on re-serialize.
	leadingHint, parseable := splitLeadingOptimizerHint(originalQuery)

	stmt, err := p.parser.Parse(parseable)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse SQL")
	}

	selectStmt, ok := stmt.(*sqlparser.Select)
	if !ok {
		return "", errors.New("only SELECT statements are supported for sharding")
	}

	shardCondition := p.buildShardCondition(shardInfo, shardRange)
	p.injectShardCondition(selectStmt, shardInfo, shardCondition)

	out := sqlparser.String(selectStmt)
	if leadingHint != "" {
		out = leadingHint + " " + out
	}
	return out, nil
}

// buildShardCondition creates the WHERE condition for sharding
func (p *SQLParser) buildShardCondition(shardInfo *ShardInfo, shardRange ShardRange) sqlparser.Expr {
	if len(shardInfo.Columns) == 1 {
		colName := shardInfo.Columns[0]
		minVal := shardRange.Mins[0]
		maxVal := shardRange.Maxs[0]

		gteExpr := &sqlparser.ComparisonExpr{
			Operator: sqlparser.GreaterEqualOp,
			Left:     sqlparser.NewColName(colName),
			Right:    p.valueToExpr(minVal),
		}

		op := sqlparser.LessThanOp
		if shardRange.IsLast {
			op = sqlparser.LessEqualOp
		}
		ltExpr := &sqlparser.ComparisonExpr{
			Operator: op,
			Left:     sqlparser.NewColName(colName),
			Right:    p.valueToExpr(maxVal),
		}

		return &sqlparser.AndExpr{
			Left:  gteExpr,
			Right: ltExpr,
		}
	}

	// Composite key: use tuple comparison
	// (col1, col2, ...) >= (val1, val2, ...)
	cols := make(sqlparser.ValTuple, len(shardInfo.Columns))
	for i, col := range shardInfo.Columns {
		cols[i] = sqlparser.NewColName(col)
	}

	minVals := make(sqlparser.ValTuple, len(shardRange.Mins))
	for i, val := range shardRange.Mins {
		minVals[i] = p.valueToExpr(val)
	}

	gteExpr := &sqlparser.ComparisonExpr{
		Operator: sqlparser.GreaterEqualOp,
		Left:     cols,
		Right:    minVals,
	}

	maxVals := make(sqlparser.ValTuple, len(shardRange.Maxs))
	for i, val := range shardRange.Maxs {
		maxVals[i] = p.valueToExpr(val)
	}

	op := sqlparser.LessThanOp
	if shardRange.IsLast {
		op = sqlparser.LessEqualOp
	}
	ltExpr := &sqlparser.ComparisonExpr{
		Operator: op,
		Left:     cols,
		Right:    maxVals,
	}

	return &sqlparser.AndExpr{
		Left:  gteExpr,
		Right: ltExpr,
	}
}

// injectShardCondition injects the shard condition into the SELECT statement
func (p *SQLParser) injectShardCondition(stmt *sqlparser.Select, shardInfo *ShardInfo, condition sqlparser.Expr) {
	if len(stmt.From) == 0 {
		return
	}

	tableAliases := p.buildTableAliasMap(stmt.From)
	shardTableAlias := p.findShardKeyTable(shardInfo.Columns, tableAliases)

	if shardTableAlias != "" {
		condition = p.qualifyCondition(condition, shardTableAlias)
	}

	if stmt.Where == nil {
		stmt.Where = &sqlparser.Where{
			Type: sqlparser.WhereClause,
			Expr: condition,
		}
	} else {
		stmt.Where.Expr = &sqlparser.AndExpr{
			Left:  stmt.Where.Expr,
			Right: condition,
		}
	}
}

// buildTableAliasMap builds a map of table aliases to table names
func (p *SQLParser) buildTableAliasMap(from []sqlparser.TableExpr) map[string]string {
	aliases := make(map[string]string)

	for _, tableExpr := range from {
		switch t := tableExpr.(type) {
		case *sqlparser.AliasedTableExpr:
			tableName := sqlparser.String(t.Expr)
			alias := tableName
			if !t.As.IsEmpty() {
				alias = t.As.String()
			}
			aliases[alias] = tableName
		case *sqlparser.JoinTableExpr:
			p.processJoinTableExpr(t, aliases)
		}
	}

	return aliases
}

// processJoinTableExpr recursively processes JOIN expressions
func (p *SQLParser) processJoinTableExpr(join *sqlparser.JoinTableExpr, aliases map[string]string) {
	if left, ok := join.LeftExpr.(*sqlparser.AliasedTableExpr); ok {
		tableName := sqlparser.String(left.Expr)
		alias := tableName
		if !left.As.IsEmpty() {
			alias = left.As.String()
		}
		aliases[alias] = tableName
	} else if leftJoin, ok := join.LeftExpr.(*sqlparser.JoinTableExpr); ok {
		p.processJoinTableExpr(leftJoin, aliases)
	}

	if right, ok := join.RightExpr.(*sqlparser.AliasedTableExpr); ok {
		tableName := sqlparser.String(right.Expr)
		alias := tableName
		if !right.As.IsEmpty() {
			alias = right.As.String()
		}
		aliases[alias] = tableName
	} else if rightJoin, ok := join.RightExpr.(*sqlparser.JoinTableExpr); ok {
		p.processJoinTableExpr(rightJoin, aliases)
	}
}

// findShardKeyTable finds which table contains the shard key columns
func (p *SQLParser) findShardKeyTable(columns []string, tableAliases map[string]string) string {
	for alias := range tableAliases {
		return alias
	}
	return ""
}

// qualifyCondition adds table qualifier to column references in the condition
func (p *SQLParser) qualifyCondition(expr sqlparser.Expr, tableAlias string) sqlparser.Expr {
	switch e := expr.(type) {
	case *sqlparser.ComparisonExpr:
		if col, ok := e.Left.(*sqlparser.ColName); ok {
			if col.Qualifier.IsEmpty() {
				col.Qualifier = sqlparser.NewTableName(tableAlias)
			}
		}
		if tuple, ok := e.Left.(sqlparser.ValTuple); ok {
			for i, expr := range tuple {
				if col, ok := expr.(*sqlparser.ColName); ok {
					if col.Qualifier.IsEmpty() {
						col.Qualifier = sqlparser.NewTableName(tableAlias)
					}
					tuple[i] = col
				}
			}
		}
	case *sqlparser.AndExpr:
		e.Left = p.qualifyCondition(e.Left, tableAlias)
		e.Right = p.qualifyCondition(e.Right, tableAlias)
	}
	return expr
}

// valueToExpr converts a Go value to a SQL expression
func (p *SQLParser) valueToExpr(val interface{}) sqlparser.Expr {
	switch v := val.(type) {
	case int:
		return sqlparser.NewIntLiteral(fmt.Sprintf("%d", v))
	case int64:
		return sqlparser.NewIntLiteral(fmt.Sprintf("%d", v))
	case float64:
		return sqlparser.NewFloatLiteral(fmt.Sprintf("%f", v))
	case string:
		return sqlparser.NewStrLiteral(v)
	case []byte:
		return sqlparser.NewStrLiteral(string(v))
	default:
		return sqlparser.NewStrLiteral(fmt.Sprintf("%v", v))
	}
}

// ExtractTableName extracts the primary table name from a query
func (p *SQLParser) ExtractTableName(query string) (string, error) {
	stmt, err := p.parser.Parse(query)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse SQL")
	}

	selectStmt, ok := stmt.(*sqlparser.Select)
	if !ok {
		return "", errors.New("only SELECT statements are supported")
	}

	if len(selectStmt.From) == 0 {
		return "", errors.New("no FROM clause found")
	}

	firstTable := selectStmt.From[0]
	if aliased, ok := firstTable.(*sqlparser.AliasedTableExpr); ok {
		return sqlparser.String(aliased.Expr), nil
	}

	return "", errors.New("could not extract table name")
}

// HasComplexFeatures checks if the query has features that might not work well with sharding
func (p *SQLParser) HasComplexFeatures(query string) (bool, string) {
	stmt, err := p.parser.Parse(query)
	if err != nil {
		return true, "unparseable query"
	}

	selectStmt, ok := stmt.(*sqlparser.Select)
	if !ok {
		return true, "not a SELECT statement"
	}

	if selectStmt.GroupBy != nil {
		return true, "GROUP BY clause"
	}
	if selectStmt.Having != nil {
		return true, "HAVING clause"
	}
	if selectStmt.Distinct {
		return true, "DISTINCT clause"
	}
	if selectStmt.Limit != nil {
		return true, "LIMIT clause"
	}

	// Check for JOIN (explicit or implicit)
	if hasJoin, desc := p.checkJoin(selectStmt.From); hasJoin {
		return true, desc
	}

	return false, ""
}

// checkJoin detects explicit JOIN and implicit multi-table FROM
func (p *SQLParser) checkJoin(from sqlparser.TableExprs) (bool, string) {
	// Implicit JOIN: FROM table1, table2 (multiple tables in FROM)
	if len(from) > 1 {
		return true, "multi-table FROM (implicit JOIN)"
	}

	// Explicit JOIN: check for JoinTableExpr in AST
	for _, tableExpr := range from {
		if p.containsJoin(tableExpr) {
			return true, "JOIN clause"
		}
	}

	return false, ""
}

// containsJoin recursively checks if a TableExpr contains a JOIN
func (p *SQLParser) containsJoin(expr sqlparser.TableExpr) bool {
	switch t := expr.(type) {
	case *sqlparser.JoinTableExpr:
		return true
	case *sqlparser.AliasedTableExpr:
		return false
	case *sqlparser.ParenTableExpr:
		for _, e := range t.Exprs {
			if p.containsJoin(e) {
				return true
			}
		}
	}
	return false
}
