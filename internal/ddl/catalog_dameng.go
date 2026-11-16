package ddl

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
)

// catalog_dameng.go reads one Dameng table's description out of the Oracle-mode
// catalogue.
//
// Six statements, in this order, all of them server-visible SQL (contract face
// ⑤) and each recorded through common.LogInteractionSQL before it goes out:
//
//  1. ALL_TABLES joined to ALL_TAB_COMMENTS — the stored name, the two shapes
//     this build refuses outright, and the table comment.
//  2. ALL_TAB_COLS — one row per column: the type word, its declared sizes, the
//     default text, and the virtual/hidden marks.
//  3. SYS.SYSCOLUMNS — the identity column, and any column carrying a mark this
//     build has not measured. It has to be a separate statement because the
//     Oracle-compatibility views do not carry the fact: measured on
//     dm8_20241022_rev244896, ALL_TAB_COLUMNS has no identity column at all and
//     ALL_TAB_IDENTITY_COLS answers `-2106 Invalid table or view name`, while
//     SYSCOLUMNS.INFO2 is `1` for the column of a table declared with IDENTITY
//     and `0` for every other column — unchanged by NOT NULL or a DEFAULT
//     clause. The seed and increment of `IDENTITY(100,5)` are not in any view
//     this build exposes (measured: no catalogue column matches %IDENT%), which
//     is why the printer prints the clause with a warning rather than a start
//     value.
//  4. ALL_COL_COMMENTS — the column comments.
//  5. ALL_CONSTRAINTS joined to ALL_CONS_COLUMNS — primary key, unique, foreign
//     key and check rules with their columns, the constraint's own status, and
//     the index a constraint already backs.
//  6. ALL_INDEXES joined to ALL_IND_COLUMNS — every index; the ones a constraint
//     backs drop out here rather than being printed twice, and so does the
//     table's own CLUSTER index, which has no MySQL spelling to print (see the
//     note in describeDameng).
//
// ALL_PART_TABLES and ALL_TAB_PARTITIONS are deliberately NOT read: ALL_TABLES
// already answers the partition question on this server (measured: PARTITIONED
// is 'YES' for a RANGE-partitioned table and 'NO' for every other, and both
// partition views agree with it), and a second statement for one fact is a
// second place to be wrong.
//
// The names are compared with UPPER() on both sides, the way internal/list
// compares them: Dameng's Oracle mode folds an unquoted identifier to upper case,
// so a lowercase --database has to find the object the server stored. The name
// the definition prints is the catalogue's own spelling, never what was typed.
//
// Nothing in here parses a type or a default. The size, precision, scale and
// default text go to mysql.go as the catalogue wrote them, so a value the
// printer cannot classify is a refusal it can name, not a decision already taken
// by a regular expression.

// dmQuery is one catalogue statement plus what it binds.
type dmQuery struct {
	query string
	args  []any
}

// run records the statement and reads it whole.
func (q dmQuery) run(ctx context.Context, db *sql.DB) ([][]string, error) {
	common.LogInteractionSQL(q.query, q.args...)
	rows, err := db.QueryContext(ctx, common.TagSQL(q.query), q.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

// describeDameng reads schema.table's description from a live Dameng server.
//
// schema is resolved by the caller: an unqualified reference means the
// connection's own schema, which on Dameng is the account the run logs in as.
func describeDameng(ctx context.Context, db *sql.DB, schema, table string) (*Table, error) {
	// 1 — the table itself.
	attrs, err := dmQuery{
		query: "SELECT T.TABLE_NAME, T.PARTITIONED, T.TEMPORARY, C.COMMENTS" +
			" FROM ALL_TABLES T LEFT JOIN ALL_TAB_COMMENTS C ON C.OWNER = T.OWNER AND C.TABLE_NAME = T.TABLE_NAME" +
			" WHERE UPPER(T.OWNER) = UPPER(?) AND UPPER(T.TABLE_NAME) = UPPER(?)",
		args: []any{schema, table},
	}.run(ctx, db)
	if err != nil {
		return nil, errors.Wrap(err, "table lookup failed")
	}
	if len(attrs) == 0 {
		return nil, ErrNotFound
	}
	out := &Table{
		Schema:      schema,
		Name:        cellText(attrs[0], 0),
		Partitioned: dmYes(attrs[0], 1),
		Temporary:   dmYes(attrs[0], 2),
	}
	if len(attrs) > 1 {
		// More than one row means the name matched twice, which on DM happens
		// when a schema and an account share the spelling; two tables of that
		// name cannot be told apart here, so the first is not silently chosen.
		return nil, errors.Errorf("more than one table named %s in schema %s", upper(table), upper(schema))
	}
	out.TableComment = cellText(attrs[0], 3)

	// 2 — the columns.
	cols, err := dmQuery{
		query: "SELECT COLUMN_NAME, DATA_TYPE, DATA_LENGTH, CHAR_LENGTH, CHAR_USED," +
			" DATA_PRECISION, DATA_SCALE, NULLABLE, DATA_DEFAULT, VIRTUAL_COLUMN, HIDDEN_COLUMN" +
			" FROM ALL_TAB_COLS WHERE UPPER(OWNER) = UPPER(?) AND UPPER(TABLE_NAME) = UPPER(?) ORDER BY COLUMN_ID",
		args: []any{schema, table},
	}.run(ctx, db)
	if err != nil {
		return nil, errors.Wrap(err, "column lookup failed")
	}
	if len(cols) == 0 {
		return nil, errors.Errorf("table %s has no columns: the catalogue cannot describe it", upper(table))
	}
	for _, row := range cols {
		out.Columns = append(out.Columns, Column{
			Name:       cellText(row, 0),
			Type:       normalizeType(cellText(row, 1)),
			ByteLength: cellInt(row, 2),
			CharLength: cellInt(row, 3),
			CharUsed:   cellText(row, 4),
			Precision:  cellNullInt(row, 5),
			Scale:      cellNullInt(row, 6),
			Nullable:   dmYes(row, 7),
			Default:    cellRaw(row, 8),
			HasDefault: strings.TrimSpace(cellRaw(row, 8)) != "",
			Virtual:    dmYes(row, 9),
			Hidden:     dmYes(row, 10),
			Comment:    "", // filled by statement 4
		})
	}

	// 3 — the identity column, and any other mark SYSCOLUMNS carries.
	marks, err := dmQuery{
		query: "SELECT C.NAME, C.INFO2 FROM SYS.SYSCOLUMNS C, SYS.SYSOBJECTS T, SYS.SYSOBJECTS S" +
			" WHERE UPPER(T.NAME) = UPPER(?) AND T.SUBTYPE$ = 'UTAB'" +
			" AND UPPER(S.NAME) = UPPER(?) AND S.ID = T.SCHID AND C.ID = T.ID AND C.INFO2 <> 0" +
			" ORDER BY C.COLID",
		args: []any{table, schema},
	}.run(ctx, db)
	if err != nil {
		return nil, errors.Wrap(err, "identity lookup failed")
	}
	for _, row := range marks {
		name := cellText(row, 0)
		idx := out.indexOf(name)
		if idx < 0 {
			return nil, errors.Errorf("column %s carries catalogue mark %s but is not in ALL_TAB_COLS", upper(name), cellText(row, 1))
		}
		if v := cellInt(row, 1); v == 1 {
			out.Columns[idx].Identity = true
		} else {
			// A mark this build has not measured. The bit is refused rather than
			// guessed at, because whatever else it says about the column is a
			// fact the printer would otherwise be printing blind.
			out.Columns[idx].Mark = strconv.Itoa(v)
		}
	}

	// 4 — the column comments.
	comments, err := dmQuery{
		query: "SELECT COLUMN_NAME, COMMENTS FROM ALL_COL_COMMENTS" +
			" WHERE UPPER(OWNER) = UPPER(?) AND UPPER(TABLE_NAME) = UPPER(?)",
		args: []any{schema, table},
	}.run(ctx, db)
	if err != nil {
		return nil, errors.Wrap(err, "column comment lookup failed")
	}
	for _, row := range comments {
		if idx := out.indexOf(cellText(row, 0)); idx >= 0 {
			out.Columns[idx].Comment = cellRaw(row, 1)
		}
	}

	// 5 — the constraints.
	cons, err := dmQuery{
		query: "SELECT A.CONSTRAINT_TYPE, A.CONSTRAINT_NAME, A.STATUS, A.INDEX_NAME," +
			" A.R_OWNER, A.R_CONSTRAINT_NAME, C.COLUMN_NAME" +
			" FROM ALL_CONSTRAINTS A LEFT JOIN ALL_CONS_COLUMNS C" +
			" ON C.OWNER = A.OWNER AND C.CONSTRAINT_NAME = A.CONSTRAINT_NAME" +
			" WHERE UPPER(A.OWNER) = UPPER(?) AND UPPER(A.TABLE_NAME) = UPPER(?) AND A.CONSTRAINT_TYPE IN ('P','U','R','C')" +
			" ORDER BY A.CONSTRAINT_NAME, C.POSITION",
		args: []any{schema, table},
	}.run(ctx, db)
	if err != nil {
		return nil, errors.Wrap(err, "constraint lookup failed")
	}
	// A constraint is one row per column, so the columns are grouped first and
	// the rules are only classified afterwards: reading the type inside the loop
	// would turn a two-column unique key into two one-column keys.
	backed := map[string]bool{}
	type consRow struct {
		kind    string
		status  string
		index   string
		refSch  string
		refCons string
		cols    []string
	}
	grouped := map[string]*consRow{}
	names := []string{}
	for _, row := range cons {
		name := cellText(row, 1)
		if name == "" {
			continue
		}
		r := grouped[name]
		if r == nil {
			r = &consRow{kind: cellText(row, 0), status: cellText(row, 2), index: cellText(row, 3),
				refSch: cellText(row, 4), refCons: cellText(row, 5)}
			grouped[name] = r
			names = append(names, name)
			if r.index != "" {
				backed[strings.ToUpper(r.index)] = true
			}
			if r.status != "" && !strings.EqualFold(r.status, "ENABLED") {
				out.DisabledConstraints = append(out.DisabledConstraints, name)
			}
		}
		if col := cellText(row, 6); col != "" {
			r.cols = append(r.cols, col)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		r := grouped[name]
		switch r.kind {
		case "P":
			if len(out.PrimaryKey) > 0 {
				return nil, errors.Errorf("table %s reports two primary keys", upper(table))
			}
			out.PrimaryKey, out.PrimaryKeyName = r.cols, name
		case "U":
			out.UniqueKeys = append(out.UniqueKeys, Key{Name: name, Columns: r.cols, Backing: r.index})
		case "R":
			out.ForeignKeys = append(out.ForeignKeys, NamedColumns{
				Name: name, Columns: r.cols, RefSchema: r.refSch, RefConstraint: r.refCons,
			})
		case "C":
			out.Checks = append(out.Checks, NamedColumns{Name: name, Columns: r.cols})
		}
	}

	// 6 — the indexes a constraint does not already back.
	ind, err := dmQuery{
		query: "SELECT I.INDEX_NAME, I.UNIQUENESS, I.INDEX_TYPE, C.COLUMN_NAME" +
			" FROM ALL_INDEXES I LEFT JOIN ALL_IND_COLUMNS C" +
			" ON C.INDEX_OWNER = I.OWNER AND C.INDEX_NAME = I.INDEX_NAME" +
			" WHERE UPPER(I.TABLE_OWNER) = UPPER(?) AND UPPER(I.TABLE_NAME) = UPPER(?) ORDER BY I.INDEX_NAME, C.COLUMN_POSITION",
		args: []any{schema, table},
	}.run(ctx, db)
	if err != nil {
		return nil, errors.Wrap(err, "index lookup failed")
	}
	indexCols := map[string][]string{}
	indexKind := map[string][2]string{}
	indexOrder := []string{}
	for _, row := range ind {
		name := strings.ToUpper(cellText(row, 0))
		if name == "" || backed[name] {
			continue
		}
		// INDEX_TYPE 'CLUSTER' is not an index the user made: it is the table's
		// own clustered storage key, and DM gives every table exactly one of them
		// whether or not a primary key was declared (measured on
		// dm8_20241022_rev244896: the whole server reports 343 CLUSTER rows against
		// 59 NORMAL ones, and each of DDL194OK, DDL194, DM140_T, DMPAR169 and the
		// constraintless T_ID_B carries precisely one). InnoDB's PRIMARY KEY is
		// the same object, so the row is dropped rather than printed: a second
		// key of the same columns would be a lie about what was ever declared,
		// and the name (INDEX33556154) is server-generated.
		//
		// Every other non-'NORMAL' spelling stays a refusal. VIRTUAL is the one
		// this server shows for a foreign key's supporting index, and no
		// functional, bitmap or domain index has been measured here, so an
		// unrecognised INDEX_TYPE is still printed as "not a plain column list"
		// instead of being guessed at.
		if strings.EqualFold(cellText(row, 2), "CLUSTER") {
			continue
		}
		if _, seen := indexKind[name]; !seen {
			indexOrder = append(indexOrder, name)
			indexKind[name] = [2]string{cellText(row, 1), cellText(row, 2)}
		}
		if col := cellText(row, 3); col != "" {
			indexCols[name] = append(indexCols[name], col)
		}
	}
	sort.Strings(indexOrder)
	for _, name := range indexOrder {
		kind := indexKind[name]
		key := Key{Name: name, Columns: indexCols[name]}
		// INDEX_TYPE 'NORMAL' is a plain column list; every other spelling is a
		// functional, bitmap, domain or cluster index, none of which has a MySQL
		// spelling this build can produce.
		if !strings.EqualFold(kind[1], "NORMAL") {
			key.Expression = true
		}
		if strings.EqualFold(kind[0], "UNIQUE") {
			out.UniqueKeys = append(out.UniqueKeys, key)
		} else {
			out.Indexes = append(out.Indexes, key)
		}
	}
	return out, nil
}

// indexOf finds a column by its stored name, case-insensitively: the catalogue
// answers a column name in one spelling and a comment or mark in another only
// across a build that folds differently, and a mismatch there is a silently
// missing comment rather than a visible error.
func (t *Table) indexOf(name string) int {
	want := strings.ToUpper(name)
	for i := range t.Columns {
		if strings.ToUpper(t.Columns[i].Name) == want {
			return i
		}
	}
	return -1
}

// scanRows reads a whole result set into text cells.
//
// Every value arrives as text on purpose: the same shape internal/list uses, and
// the one the Dameng driver has been measured to answer for DEC, TEXT and CHAR
// alike. A number is then parsed by the caller that needs it as a number, so a
// NULL and a zero stay distinguishable in the print.
func scanRows(rows *sql.Rows) ([][]string, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		values := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := make([]string, len(cols))
		for i, v := range values {
			if v.Valid {
				row[i] = v.String
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// cellText returns one cell with its padding removed — the reading a name or a
// flag wants, since a CHAR(1) flag cell arrives as "Y " on some builds.
func cellText(row []string, i int) string {
	return strings.TrimSpace(cellRaw(row, i))
}

// cellRaw returns one cell as the server wrote it. A default text keeps its
// spaces: `((CODE || '-') || CODE)` and `'A B'` are both meaningful exactly as
// they stand, and a trimmed value is a value that has already been edited.
func cellRaw(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return row[i]
}

// cellNullInt answers the two-part question a catalogue number asks: was it
// written at all, and what is it. An empty cell is "not stated" — which for
// DATA_PRECISION is a real fact about the column, not a missing value.
func cellNullInt(row []string, i int) sql.NullInt64 {
	text := cellText(row, i)
	if text == "" {
		return sql.NullInt64{}
	}
	// A DEC cell can arrive with a sign or a fraction; strconv.ParseInt takes
	// neither, so the text is reduced first rather than refused.
	if j := strings.IndexByte(text, '.'); j >= 0 {
		text = text[:j]
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

// cellInt reads a catalogue number whose absence the caller has already decided
// is a refusal; 0 is what an unreadable or missing cell gives, and the printer
// treats a size of 0 on a sized type as the refusal it is.
func cellInt(row []string, i int) int {
	v := cellNullInt(row, i)
	if !v.Valid {
		return 0
	}
	return int(v.Int64)
}

// dmYes reports whether a Dameng catalogue flag cell says yes. The views write
// 'Y'/'N' for NULLABLE and PARTITIONED and 'YES'/'NO' for VIRTUAL_COLUMN and
// HIDDEN_COLUMN (measured on the same server, in the same query set), so both
// spellings mean yes and anything else means no — which is what an unrecognised
// spelling of a refusal-shaped flag has to mean.
func dmYes(row []string, i int) bool {
	text := strings.ToUpper(cellText(row, i))
	return text == "Y" || text == "YES"
}

// normalizeType folds a catalogue type word to the vocabulary the printer keys
// on (see backend.NormalizeDamengTypeName).
func normalizeType(name string) string { return backend.NormalizeDamengTypeName(name) }

// upper is the reading every Dameng identifier in a message gets: the server
// folds an unquoted name to upper case, and a message that prints what was typed
// sends the operator looking for an object under a name the catalogue never had.
func upper(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }
