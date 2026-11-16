package ddl

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/config"
)

// num is a catalogue number that was written: sql.NullInt64's valid half, spelled
// out because a test that reads `Scale: num(0)` and one that reads `Scale:
// sql.NullInt64{}` are asking different questions about the same column.
func num(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func TestMySQLRulesCoverDamengTypes(t *testing.T) {
	// Both directions are the point: a type the data path learned must not become
	// a silent fall-through in the DDL path, and a DDL rule that names a word the
	// data path has never heard of is a rule no catalogue can ever reach.
	for _, name := range backend.DamengTypeNames() {
		if _, ok := mysqlRules[name]; !ok {
			t.Errorf("Dameng type %q has no MySQL decision: add a rule or a recorded refusal", name)
		}
	}
	for name := range mysqlRules {
		if !slices.Contains(backend.DamengTypeNames(), name) {
			t.Errorf("mysqlRules keys %q, which the Dameng data path does not know: the two vocabularies have drifted", name)
		}
	}
}

func TestColumnTypeWords(t *testing.T) {
	for _, tc := range []struct {
		name string
		col  Column
		want string
	}{
		{"int", Column{Name: "A", Type: "INT"}, "int"},
		{"integer is dm's int", Column{Name: "A", Type: "INTEGER"}, "int"},
		{"bigint", Column{Name: "A", Type: "BIGINT"}, "bigint"},
		{"smallint", Column{Name: "A", Type: "SMALLINT"}, "smallint"},
		{"tinyint", Column{Name: "A", Type: "TINYINT"}, "tinyint"},
		{"bit is one digit", Column{Name: "A", Type: "BIT"}, "tinyint"},
		{"boolean is mysql's boolean", Column{Name: "A", Type: "BOOLEAN"}, "tinyint(1)"},
		{"float widens", Column{Name: "A", Type: "FLOAT"}, "double"},
		{"real widens", Column{Name: "A", Type: "REAL"}, "double"},
		{"binary float stays four bytes", Column{Name: "A", Type: "BINARY_FLOAT"}, "float"},
		{"char", Column{Name: "A", Type: "CHAR", CharLength: 4}, "char(4)"},
		{"varchar", Column{Name: "A", Type: "VARCHAR", CharLength: 255}, "varchar(255)"},
		{"varchar2 prints as varchar", Column{Name: "A", Type: "VARCHAR2", CharLength: 20}, "varchar(20)"},
		{"nvarchar2 is already characters", Column{Name: "A", Type: "NVARCHAR2", CharLength: 1020}, "varchar(1020)"},
		{"varchar past the row limit widens", Column{Name: "A", Type: "VARCHAR", CharLength: 32767}, "longtext"},
		{"clob", Column{Name: "A", Type: "CLOB"}, "longtext"},
		{"long varchar", Column{Name: "A", Type: "LONG VARCHAR"}, "longtext"},
		{"varbinary", Column{Name: "A", Type: "VARBINARY", ByteLength: 64}, "varbinary(64)"},
		{"raw prints as varbinary", Column{Name: "A", Type: "RAW", ByteLength: 16}, "varbinary(16)"},
		{"varbinary past the limit widens", Column{Name: "A", Type: "VARBINARY", ByteLength: 70000}, "longblob"},
		{"binary", Column{Name: "A", Type: "BINARY", ByteLength: 4}, "binary(4)"},
		{"blob", Column{Name: "A", Type: "BLOB"}, "longblob"},
		{"image", Column{Name: "A", Type: "IMAGE"}, "longblob"},
		{"dm date carries a clock", Column{Name: "A", Type: "DATE"}, "datetime"},
		{"datetime with fraction", Column{Name: "A", Type: "DATETIME", Scale: num(6)}, "datetime(6)"},
		{"datetime without a scale is seconds", Column{Name: "A", Type: "TIMESTAMP"}, "datetime"},
		{"time with fraction", Column{Name: "A", Type: "TIME", Scale: num(6)}, "time(6)"},
		{"number with a fraction", Column{Name: "A", Type: "NUMBER", Precision: num(10), Scale: num(2)}, "decimal(10,2)"},
		{"number three digits", Column{Name: "A", Type: "NUMBER", Precision: num(3), Scale: num(0)}, "tinyint"},
		{"number five digits", Column{Name: "A", Type: "NUMBER", Precision: num(5), Scale: num(0)}, "smallint"},
		{"number seven digits", Column{Name: "A", Type: "NUMBER", Precision: num(7), Scale: num(0)}, "mediumint"},
		{"number nine digits", Column{Name: "A", Type: "NUMBER", Precision: num(9), Scale: num(0)}, "int"},
		{"number ten digits is past int", Column{Name: "A", Type: "NUMBER", Precision: num(10), Scale: num(0)}, "bigint"},
		{"number eighteen digits", Column{Name: "A", Type: "NUMBER", Precision: num(18), Scale: num(0)}, "bigint"},
		{"number thirty-eight digits", Column{Name: "A", Type: "NUMBER", Precision: num(38), Scale: num(0)}, "decimal(38,0)"},
		{"numeric prints as decimal", Column{Name: "A", Type: "NUMERIC", Precision: num(18), Scale: num(4)}, "decimal(18,4)"},
	} {
		got, err := columnType(tc.col)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestColumnTypeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		col  Column
		want string
	}{
		{
			name: "date with an offset",
			col:  Column{Name: "TSZ", Type: "DATETIME WITH TIME ZONE"},
			want: "has no datetime type that carries an offset",
		},
		{
			name: "local time zone",
			col:  Column{Name: "TSLZ", Type: "TIMESTAMP WITH LOCAL TIME ZONE"},
			want: "has no timestamp type that carries an offset",
		},
		{
			name: "year is not a measured dm type",
			col:  Column{Name: "Y", Type: "YEAR"},
			want: "has not been measured on Dameng",
		},
		{
			name: "unconstrained number",
			col:  Column{Name: "NM", Type: "NUMBER", Scale: num(0)},
			want: "declared without a precision",
		},
		{
			name: "negative scale",
			col:  Column{Name: "NM", Type: "NUMBER", Precision: num(10), Scale: num(-2)},
			want: "reads as a shift to the left",
		},
		{
			name: "scale past precision",
			col:  Column{Name: "NM", Type: "NUMBER", Precision: num(4), Scale: num(8)},
			want: "MySQL's DECIMAL holds at most",
		},
		{
			name: "precision past decimal",
			col:  Column{Name: "NM", Type: "NUMBER", Precision: num(70), Scale: num(0)},
			want: "past MySQL's DECIMAL maximum",
		},
		{
			name: "char past the inline limit",
			col:  Column{Name: "C", Type: "CHAR", CharLength: 300},
			want: "a longer fixed column is a different length",
		},
		{
			name: "varchar with no declared size",
			col:  Column{Name: "V", Type: "VARCHAR"},
			want: "with no declared size",
		},
		{
			name: "binary past its limit",
			col:  Column{Name: "B", Type: "BINARY", ByteLength: 300},
			want: "MySQL's BINARY stops at 255",
		},
		{
			name: "fraction past six",
			col:  Column{Name: "D", Type: "DATETIME", Scale: num(7)},
			want: "holds 0 to 6",
		},
		{
			name: "a type word neither path knows",
			col:  Column{Name: "X", Type: "INTERVAL DAY TO SECOND"},
			want: "no MySQL spelling for",
		},
	} {
		_, err := columnType(tc.col)
		if err == nil {
			t.Errorf("%s: columnType succeeded, want a refusal", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %q, want it to say %q", tc.name, err, tc.want)
		}
	}
}

func TestDefaultClause(t *testing.T) {
	for _, tc := range []struct {
		name string
		col  Column
		typ  string
		want string
		err  string
	}{
		{name: "no default", col: Column{Name: "A"}, typ: "int", want: ""},
		{name: "number", col: Column{Name: "A", HasDefault: true, Default: "7"}, typ: "int", want: "DEFAULT 7"},
		{name: "decimal", col: Column{Name: "A", HasDefault: true, Default: "3.14"}, typ: "decimal(10,2)", want: "DEFAULT 3.14"},
		{name: "string", col: Column{Name: "A", HasDefault: true, Default: "'AB'"}, typ: "varchar(10)", want: "DEFAULT 'AB'"},
		{name: "string with an embedded quote", col: Column{Name: "A", HasDefault: true, Default: "'A''B'"}, typ: "varchar(10)", want: "DEFAULT 'A''B'"},
		{name: "national string", col: Column{Name: "A", HasDefault: true, Default: "N'x'"}, typ: "varchar(10)", want: "DEFAULT N'x'"},
		{name: "null on a nullable column", col: Column{Name: "A", Nullable: true, HasDefault: true, Default: "NULL"}, typ: "int", want: "DEFAULT NULL"},
		{name: "current timestamp on a datetime", col: Column{Name: "A", HasDefault: true, Default: "CURRENT_TIMESTAMP"}, typ: "datetime(6)", want: "DEFAULT CURRENT_TIMESTAMP"},
		{name: "current timestamp on a time", col: Column{Name: "A", HasDefault: true, Default: "CURRENT_TIMESTAMP"}, typ: "time(6)", want: "DEFAULT CURRENT_TIMESTAMP"},
		{
			name: "current timestamp on a number",
			col:  Column{Name: "N", HasDefault: true, Default: "CURRENT_TIMESTAMP"},
			typ:  "int",
			err:  "MySQL evaluates only for a date or time column",
		},
		{
			name: "null on a not null column",
			col:  Column{Name: "N", HasDefault: true, Default: "NULL"},
			typ:  "int",
			err:  "NOT NULL and defaults to NULL",
		},
		{
			name: "an expression default",
			col:  Column{Name: "FULLNAME", HasDefault: true, Default: "((CODE || '-') || CODE)"},
			typ:  "varchar(50)",
			err:  "not a literal this build prints",
		},
		{
			name: "a function default that is not allow-listed",
			col:  Column{Name: "CREATED", HasDefault: true, Default: "SYSDATE"},
			typ:  "datetime",
			err:  "not a literal this build prints",
		},
	} {
		got, err := defaultClause(tc.col, tc.typ)
		if tc.err != "" {
			if err == nil {
				t.Errorf("%s: defaultClause succeeded, want %q", tc.name, tc.err)
			} else if !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: got %q, want it to say %q", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRenderMySQLGolden(t *testing.T) {
	text, err := RenderMySQL(&Table{
		Schema: "IMEXIT",
		Name:   "DDL194OK",
		Columns: []Column{
			{Name: "ID", Type: "INT", Nullable: false, Identity: true},
			{Name: "CODE", Type: "VARCHAR", CharLength: 10, CharUsed: "B", Nullable: false, Default: "'AB'", HasDefault: true},
			{Name: "AMT", Type: "NUMBER", Precision: num(10), Scale: num(2), Default: "3.14", HasDefault: true, Nullable: true},
			{Name: "CREATED", Type: "DATETIME", Scale: num(6), Default: "CURRENT_TIMESTAMP", HasDefault: true, Nullable: true},
			{Name: "REMARK", Type: "VARCHAR", CharLength: 64, Nullable: true, Comment: "it's 64 bytes wide"},
		},
		PrimaryKey:     []string{"ID"},
		PrimaryKeyName: "CONS134218899",
		UniqueKeys:     []Key{{Name: "UK194OK", Columns: []string{"CODE"}}},
		Indexes:        []Key{{Name: "IDX194OK", Columns: []string{"AMT", "CREATED"}}},
		TableComment:   `ok "round" 194`,
	})
	if err != nil {
		t.Fatalf("RenderMySQL: %v", err)
	}
	want := strings.Join([]string{
		"CREATE TABLE `IMEXIT`.`DDL194OK` (",
		"  `ID` int NOT NULL AUTO_INCREMENT,",
		"  `CODE` varchar(10) NOT NULL DEFAULT 'AB',",
		"  `AMT` decimal(10,2) DEFAULT 3.14,",
		"  `CREATED` datetime(6) DEFAULT CURRENT_TIMESTAMP,",
		"  `REMARK` varchar(64) COMMENT 'it''s 64 bytes wide',",
		"  PRIMARY KEY (`ID`),",
		"  UNIQUE KEY `UK194OK` (`CODE`),",
		"  KEY `IDX194OK` (`AMT`, `CREATED`)",
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='ok \"round\" 194'",
	}, "\n")
	if text != want {
		t.Errorf("printed:\n%s\nwant:\n%s", text, want)
	}
}

func TestRenderMySQLRefusals(t *testing.T) {
	base := func() *Table {
		return &Table{
			Name:       "T",
			Columns:    []Column{{Name: "A", Type: "INT", Nullable: false}, {Name: "B", Type: "VARCHAR", CharLength: 10, Nullable: true}},
			PrimaryKey: []string{"A"},
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Table)
		want   string
	}{
		{
			name:   "partitioned",
			mutate: func(t *Table) { t.Partitioned = true },
			want:   "is a partitioned table",
		},
		{
			name:   "temporary",
			mutate: func(t *Table) { t.Temporary = true },
			want:   "is a temporary table",
		},
		{
			name: "foreign key",
			mutate: func(t *Table) {
				t.ForeignKeys = []NamedColumns{{Name: "FK_CH", Columns: []string{"A"}, RefConstraint: "PK_P"}}
			},
			want: "carries a referential constraint",
		},
		{
			name: "check constraint",
			mutate: func(t *Table) {
				t.Checks = []NamedColumns{{Name: "CK_A", Columns: []string{"A"}}}
			},
			want: "carries a check constraint",
		},
		{
			name:   "disabled constraint",
			mutate: func(t *Table) { t.DisabledConstraints = []string{"CK_A"} },
			want:   "not enabled",
		},
		{
			name: "generated column",
			mutate: func(t *Table) {
				t.Columns[1].Virtual = true
				t.Columns[1].Default = "((A || '-'))"
				t.Columns[1].HasDefault = true
			},
			want: "is generated from",
		},
		{
			name:   "hidden column",
			mutate: func(t *Table) { t.Columns[1].Hidden = true },
			want:   "hidden catalogue column",
		},
		{
			name:   "unmeasured catalogue mark",
			mutate: func(t *Table) { t.Columns[1].Mark = "2" },
			want:   "carries catalogue mark 2",
		},
		{
			name:   "identity that is not the key",
			mutate: func(t *Table) { t.Columns[1] = Column{Name: "B", Type: "INT", Nullable: true, Identity: true} },
			want:   "is not the whole primary key",
		},
		{
			name: "two identity columns",
			mutate: func(t *Table) {
				t.Columns[0].Identity = true
				t.Columns[1] = Column{Name: "B", Type: "INT", Identity: true}
			},
			want: "MySQL allows one auto-increment column",
		},
		{
			name: "identity on a character column",
			mutate: func(t *Table) {
				t.Columns[0].Identity = true
				t.Columns[0].Type = "VARCHAR"
				t.Columns[0].CharLength = 10
			},
			want: "which MySQL cannot generate",
		},
		{
			name: "index over a wide varchar",
			mutate: func(t *Table) {
				t.Columns[1].CharLength = 1000
				t.Indexes = []Key{{Name: "IDX_W", Columns: []string{"B"}}}
			},
			want: "InnoDB allows 3072",
		},
		{
			name: "index over a large text column",
			mutate: func(t *Table) {
				t.Columns[1] = Column{Name: "B", Type: "CLOB", Nullable: true}
				t.Indexes = []Key{{Name: "IDX_T", Columns: []string{"B"}}}
			},
			want: "index it on a prefix instead",
		},
		{
			name:   "expression index",
			mutate: func(t *Table) { t.Indexes = []Key{{Name: "IDX_F", Columns: []string{"A"}, Expression: true}} },
			want:   "not a plain column list",
		},
		{
			name:   "index naming a column that is not there",
			mutate: func(t *Table) { t.Indexes = []Key{{Name: "IDX_X", Columns: []string{"NOPE"}}} },
			want:   "is not in the table's column list",
		},
		{
			name:   "index name past the target limit",
			mutate: func(t *Table) { t.Indexes = []Key{{Name: strings.Repeat("I", 65), Columns: []string{"A"}}} },
			want:   "MySQL's limit for an index name",
		},
	} {
		tab := base()
		tc.mutate(tab)
		_, err := RenderMySQL(tab)
		if err == nil {
			t.Errorf("%s: RenderMySQL succeeded, want a refusal", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %q, want it to say %q", tc.name, err, tc.want)
		}
	}
}

func TestConvertRefusesEveryPairButDmMySQL(t *testing.T) {
	// The pair table and describe's switch must not drift apart: an engine the
	// switch learned must be one SupportedPair also allows, and the other way
	// round. Convert is called with no connection because the question is about
	// the pair, and it is answered before anything is read.
	for _, engine := range backend.Engines() {
		for _, name := range config.SQLDialects() {
			target := config.SQLDialect(name)
			err := callConvert(engine, target)
			supported := SupportedPair(engine, target)
			if supported {
				if err != nil && strings.Contains(err.Error(), "is not supported") {
					t.Errorf("%s to %s: SupportedPair says yes, Convert says %v", engine, name, err)
				}
				continue
			}
			if err == nil || !strings.Contains(err.Error(), "is not supported") {
				t.Errorf("%s to %s: expected a pair refusal, got %v", engine, name, err)
			}
		}
	}
}

// callConvert runs Convert where a nil connection would be reached only if the
// pair had already been accepted, and reports the refusal rather than the panic.
func callConvert(engine string, target config.SQLDialect) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = nil // the pair was accepted and the read started
		}
	}()
	_, err = Convert(context.Background(), nil, engine, "S", "T", target)
	return err
}

func TestAskedReader(t *testing.T) {
	// auto and the engine's own grammar are not conversions: the definition the
	// server returns already is that grammar, and refusing it would refuse the
	// identity map.
	for _, tc := range []struct {
		engine string
		want   bool
	}{
		{"dm", true},
		{"mysql", false},
		{"pg", true},
		{"gauss", true},
	} {
		if got := Asked(tc.engine, config.SQLDialectMySQL); got != tc.want {
			t.Errorf("Asked(%q, mysql) = %v, want %v", tc.engine, got, tc.want)
		}
	}
	for _, tc := range []struct {
		engine string
		want   bool
	}{
		{"dm", false},
		{"mysql", true},
		{"pg", true},
		{"gauss", true},
	} {
		if got := Asked(tc.engine, config.SQLDialectDM); got != tc.want {
			t.Errorf("Asked(%q, dm) = %v, want %v", tc.engine, got, tc.want)
		}
	}
	if Asked("dm", config.SQLDialectAuto) || Asked("mysql", config.SQLDialectAuto) {
		t.Error("auto asked for a conversion, which is what every released run meant")
	}
}
