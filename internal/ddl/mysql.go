package ddl

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/imex/internal/common"
)

// mysql.go prints one catalogue-described table in MySQL grammar.
//
// The rule that decides every row of mysqlRules below is the one that makes a
// catalogue rebuild trustworthy: a source fact that cannot reach the target
// grammar is a REFUSAL that names the object, never a silent omission and never
// an approximation. Two directions are allowed without a refusal, because they
// cannot lose a value:
//
//   - widening a number or a size (REAL to DOUBLE, a byte-denominated VARCHAR(20)
//     to a MySQL VARCHAR(20), which holds twenty characters and so at least as
//     many bytes as the source held);
//   - restating a type the target spells differently but means identically
//     (DM DATE, which carries a clock, as DATETIME).
//
// Everything else — an offset-bearing timestamp, a functional index, a
// generation expression, an unmeasured catalogue mark — stops the run. That is
// the whole reason this path reads the catalogue instead of rewriting the
// server's DDL text: a rewrite can only discover what it dropped afterwards.
//
// The sizes, precisions and default texts arrive exactly as the catalogue wrote
// them (see catalog_dameng.go), so the questions below are asked of the server's
// own numbers, not of a parser's guess about them.

// limit is one MySQL ceiling, kept as a named constant because a refusal that
// says "too long" has to say too long for what.
type limit int

const (
	charMax         limit = 255   // CHAR(n), BINARY(n): MySQL's inline-storage ceiling
	varcharMax      limit = 16383 // VARCHAR(n) in a utf8mb4 table: the 65535-byte row limit
	varbinaryMax    limit = 65535 // VARBINARY(n)
	decimalMax      limit = 65    // DECIMAL(p)
	decimalScaleMax limit = 30    // DECIMAL(p,s)
	keyNameMax      limit = 64    // an index or constraint name
	fspMax          limit = 6     // the fractional-seconds precision a time type takes
)

// family is what a source type word needs from the printer: a fixed spelling, a
// size, a precision and scale, or a refusal.
type family int

const (
	famInt         family = iota // a fixed MySQL word that takes no size
	famDecimal                   // an exact number: precision and scale decide the spelling
	famFloat                     // an approximate number, printed wider than declared
	famBit                       // a one-digit boolean-ish column
	famChar                      // fixed character text, sized in characters
	famVarchar                   // variable character text, sized in characters
	famText                      // large character text: no size reaches the target
	famBinarySized               // fixed or variable raw bytes, sized in bytes
	famLargeBinary               // large raw bytes
	famDatetime                  // a date and clock, whose scale is a fractional precision
	famTime                      // a clock only, whose scale is a fractional precision
	famRefuse                    // never printed; reason says why
)

// mysqlRule is one source type word's printing decision.
type mysqlRule struct {
	// word is the MySQL type to print. For famDecimal and the sized families it is
	// the base spelling the size is added to; for famInt, famFloat, famBit,
	// famText and famLargeBinary it is the whole answer; for famRefuse it is
	// empty.
	word string
	// family is how much of the column the printer still has to look at.
	family family
	// reason is why famRefuse stops the run. It goes into the message verbatim,
	// so it states the fact an operator can act on, not the type's name.
	reason string
}

// mysqlRules keys every Dameng type word the data path knows (backend.damengTypes)
// onto a MySQL decision. The keys are the normalised spellings — upper case,
// single-spaced, no size suffix — which is the same vocabulary the catalogue
// answers in after normalizeType.
//
// TestMySQLRulesCoverDamengTypes walks backend.DamengTypeNames() and fails if a
// word is missing here, so a type added for the DEL path cannot be printed as a
// refusal by accident or, worse, fall through to a default.
var mysqlRules = map[string]mysqlRule{
	// Exact numbers. The declared precision and scale decide everything, so the
	// word only picks the rule; an unconstrained NUMBER is refused in columnType
	// because "any value the server can hold" has no MySQL spelling.
	"NUMBER":  {word: "decimal", family: famDecimal},
	"DECIMAL": {word: "decimal", family: famDecimal},
	"DEC":     {word: "decimal", family: famDecimal},
	"NUMERIC": {word: "decimal", family: famDecimal},

	// Integers: the target has the same words, so the sizes carry over as they
	// are. INTEGER is DM's spelling of INT.
	"INTEGER":  {word: "int", family: famInt},
	"INT":      {word: "int", family: famInt},
	"SMALLINT": {word: "smallint", family: famInt},
	"TINYINT":  {word: "tinyint", family: famInt},
	"BIGINT":   {word: "bigint", family: famInt},

	// Approximate numbers, printed at least as wide as the source. REAL and FLOAT
	// are 4-byte on DM8 (measured: LENGTH$ 4 for both in a type-coverage table),
	// and MySQL's FLOAT is 4-byte too — but MySQL's own FLOAT holds exactly what
	// a DM FLOAT holds only if the fraction types match, and DOUBLE always holds
	// a FLOAT's values, so the widening is the safe direction and the storage
	// cost is the documented price. BINARY_FLOAT is DM's 4-byte word and prints
	// as MySQL's 4-byte word.
	"FLOAT":            {word: "double", family: famFloat},
	"REAL":             {word: "double", family: famFloat},
	"DOUBLE":           {word: "double", family: famFloat},
	"DOUBLE PRECISION": {word: "double", family: famFloat},
	"BINARY_FLOAT":     {word: "float", family: famFloat},
	"BINARY_DOUBLE":    {word: "double", family: famFloat},

	// One-digit booleans. MySQL has no boolean type of its own — BOOLEAN is
	// TINYINT(1) there — and this tool already reads a MySQL boolean as
	// tinyint(1) (see Backend.BooleanFlags), so a DM BOOLEAN prints in exactly
	// the spelling the import side expects. DM's BIT is a bit-string rather than
	// a boolean and the dump already writes it as one digit, so it prints as a
	// plain tinyint.
	"BIT":     {word: "tinyint", family: famInt},
	"BOOLEAN": {word: "tinyint(1)", family: famInt},

	// Character text. The declared size is the catalogue's CHAR_LENGTH, which is
	// the number the column's own definition wrote; CHAR_USED says whether that
	// number meant bytes or characters, and the byte case widens (see the header).
	"CHAR":      {word: "char", family: famChar},
	"CHARACTER": {word: "char", family: famChar},
	"NCHAR":     {word: "char", family: famChar},
	"VARCHAR":   {word: "varchar", family: famVarchar},
	"VARCHAR2":  {word: "varchar", family: famVarchar},
	"NVARCHAR2": {word: "varchar", family: famVarchar},

	// Large character text. DM's longest text word holds 2147483647 bytes
	// (measured: DATA_LENGTH on a CLOB and on a TEXT column), which is MySQL's
	// LONGTEXT, so no declared size is lost by taking the one spelling.
	"CLOB":         {word: "longtext", family: famText},
	"NCLOB":        {word: "longtext", family: famText},
	"TEXT":         {word: "longtext", family: famText},
	"LONG":         {word: "longtext", family: famText},
	"LONGVARCHAR":  {word: "longtext", family: famText},
	"LONG VARCHAR": {word: "longtext", family: famText},

	// Raw bytes. VARBINARY and RAW are DM's sized byte words, BINARY its fixed
	// one; past MySQL's byte ceiling the sized words become LONGBLOB, which is
	// the only target word that can hold the same payload.
	"BINARY":         {word: "binary", family: famBinarySized},
	"VARBINARY":      {word: "varbinary", family: famBinarySized},
	"RAW":            {word: "varbinary", family: famBinarySized},
	"BLOB":           {word: "longblob", family: famLargeBinary},
	"IMAGE":          {word: "longblob", family: famLargeBinary},
	"LONGVARBINARY":  {word: "longblob", family: famLargeBinary},
	"LONG VARBINARY": {word: "longblob", family: famLargeBinary},
	"LONG RAW":       {word: "longblob", family: famLargeBinary},

	// Date and clock. DM's Oracle-mode DATE carries a time of day (measured, and
	// the reason the dump formats it with a clock), so it prints as MySQL's
	// DATETIME rather than as MySQL's DATE, which would drop it. The scale on a
	// DM datetime or time word is a fractional-seconds precision, which MySQL
	// spells in the same place: DATETIME(6), TIME(6).
	"DATE":      {word: "datetime", family: famDatetime},
	"DATETIME":  {word: "datetime", family: famDatetime},
	"DATETIME2": {word: "datetime", family: famDatetime},
	"TIMESTAMP": {word: "datetime", family: famDatetime},
	"TIME":      {word: "time", family: famTime},

	// Refusals. Each one is a fact the target grammar has nowhere to put.
	"TIME WITH TIME ZONE":            {family: famRefuse, reason: "MySQL's TIME holds no offset, so a Dameng time-with-zone column has nowhere to put the part that identifies the instant"},
	"DATETIME WITH TIME ZONE":        {family: famRefuse, reason: "MySQL has no datetime type that carries an offset, and dropping one changes what an instant means"},
	"TIMESTAMP WITH TIME ZONE":       {family: famRefuse, reason: "MySQL has no timestamp type that carries an offset, and converting to UTC is a decision about the data, not about the definition"},
	"TIMESTAMP WITH LOCAL TIME ZONE": {family: famRefuse, reason: "MySQL has no timestamp type that carries an offset, and converting to UTC is a decision about the data, not about the definition"},
	// YEAR is in the data path's table only so that a catalogue word naming the
	// MySQL type cannot fall into KindUnknown. No Dameng server was measured
	// storing it, so this printer has no basis for a decision about it.
	"YEAR": {family: famRefuse, reason: "YEAR is a MySQL type word that has not been measured on Dameng, so nothing here can say what a Dameng YEAR column holds"},
}

// literalDefault matches a catalogue default text that is already a MySQL
// literal: a number, or a (national) quoted string whose embedded quotes are the
// doubled form both dialects use.
var (
	numericDefault   = regexp.MustCompile(`^[-+]?[0-9]+(\.[0-9]*)?([eE][-+]?[0-9]+)?$`)
	stringDefault    = regexp.MustCompile(`^[Nn]?'(?:[^']|'')*'$`)
	timestampDefault = regexp.MustCompile(`(?i)^CURRENT_TIMESTAMP(\([0-9]\))?$`)
)

// indexKeyMaxBytes and utf8mb4Bytes are the two numbers behind the index refusal.
// InnoDB's key limit on the servers this build was tested against is 3072 bytes,
// and a utf8mb4 character reserves four of them, so a key column whose declared
// size passes 768 characters is rejected by the target server — which the run
// reports here, with both numbers, instead of handing the operator a CREATE that
// fails on replay.
const (
	indexKeyMaxBytes = 3072
	utf8mb4Bytes     = 4
)

// RenderMySQL prints t as one MySQL CREATE TABLE statement, without its
// terminating semicolon: show adds that, exactly as it does for a definition
// copied from the server.
//
// The refusal order is the order a reader wants the surprises in: the table's own
// shape first (a partitioned table is not a table this statement can be), then
// the columns left to right in the order the catalogue reported them, then the
// keys. Printing stops at the first refusal, so a run either produces a
// statement or names one thing that needs a decision.
func RenderMySQL(t *Table) (string, error) {
	if err := refuseTableShape(t); err != nil {
		return "", err
	}
	autoInc, err := identityColumn(t)
	if err != nil {
		return "", err
	}
	cols := make([]string, 0, len(t.Columns)+4)
	for i, c := range t.Columns {
		line, err := columnLine(c, i == autoInc)
		if err != nil {
			return "", err
		}
		cols = append(cols, line)
	}
	keys, err := keyLines(t)
	if err != nil {
		return "", err
	}
	cols = append(cols, keys...)

	var b strings.Builder
	b.WriteString("CREATE TABLE " + qualifyMySQL(t) + " (\n")
	b.WriteString("  " + strings.Join(cols, ",\n  "))
	b.WriteString("\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	if t.TableComment != "" {
		text, err := mysqlString(t.TableComment)
		if err != nil {
			return "", errors.Wrapf(err, "table comment cannot be printed")
		}
		b.WriteString(" COMMENT=" + text)
	}
	return b.String(), nil
}

// qualifyMySQL prints the table reference. An empty schema leaves the name bare,
// which is what `show tab t -D mydb --sql-dialect mysql` means: the statement
// then creates the table in whatever database the replay runs against.
func qualifyMySQL(t *Table) string {
	if t.Schema == "" {
		return mysqlIdent(t.Name)
	}
	return mysqlIdent(t.Schema) + "." + mysqlIdent(t.Name)
}

// refuseTableShape is the table-level part of the refusal list, checked before a
// single column is printed.
func refuseTableShape(t *Table) error {
	if t.Partitioned {
		return errors.Errorf("%s is a partitioned table: the partition rule is not printed, so a MySQL copy of its columns would not be the same table", qualifyMySQL(t))
	}
	if t.Temporary {
		return errors.Errorf("%s is a temporary table: MySQL's temporary table is a session object with different rules, so the printed definition would not mean the same thing", qualifyMySQL(t))
	}
	if len(t.ForeignKeys) > 0 {
		names := make([]string, 0, len(t.ForeignKeys))
		for _, fk := range t.ForeignKeys {
			names = append(names, fk.Name+" -> "+fk.RefConstraint)
		}
		return errors.Errorf("%s carries a referential constraint (%s): show prints one table, and a foreign key would point at a table this statement does not create",
			qualifyMySQL(t), strings.Join(names, ", "))
	}
	if len(t.Checks) > 0 {
		names := make([]string, 0, len(t.Checks))
		for _, ck := range t.Checks {
			names = append(names, ck.Name)
		}
		return errors.Errorf("%s carries a check constraint (%s): a Dameng check condition is Oracle grammar, and this build does not rewrite expressions",
			qualifyMySQL(t), strings.Join(names, ", "))
	}
	if len(t.DisabledConstraints) > 0 {
		return errors.Errorf("%s carries a constraint that is not enabled (%s): printing it would add a rule the source does not enforce, and leaving it out would print a freer table",
			qualifyMySQL(t), strings.Join(t.DisabledConstraints, ", "))
	}
	return nil
}

// identityColumn finds the column that earns MySQL's AUTO_INCREMENT, and refuses
// every other shape an IDENTITY column can arrive in.
//
// The one it prints has to be the whole primary key by itself, for two reasons
// that are the target's own: MySQL allows one auto-increment column and it must
// be the first column of a key, and a composite primary key would make the
// generated value part of a rule this statement cannot set up in one clause.
//
// The seed and increment are a documented gap rather than a refusal: no view on
// the measured DM8 build reports them (see catalog_dameng.go), so the printed
// table counts from 1 and the run warns that the counter, unlike the columns,
// was not read from the source.
func identityColumn(t *Table) (int, error) {
	at := -1
	for i, c := range t.Columns {
		if !c.Identity {
			continue
		}
		if at >= 0 {
			return 0, errors.Errorf("%s declares IDENTITY on both %s and %s, and MySQL allows one auto-increment column per table",
				qualifyMySQL(t), mysqlIdent(t.Columns[at].Name), mysqlIdent(c.Name))
		}
		at = i
	}
	if at < 0 {
		return -1, nil
	}
	c := t.Columns[at]
	word, err := columnType(c)
	if err != nil {
		return -1, err
	}
	if !integerWord(word) {
		return 0, errors.Errorf("column %s is an IDENTITY column of type %s, which MySQL cannot generate: the target needs an integer type",
			mysqlIdent(c.Name), c.Type)
	}
	if len(t.PrimaryKey) != 1 || !strings.EqualFold(t.PrimaryKey[0], c.Name) {
		return 0, errors.Errorf("column %s is an IDENTITY column but is not the whole primary key: MySQL requires an auto-increment column to stand alone in the key",
			mysqlIdent(c.Name))
	}
	common.Logger.Warn("identity seed not read",
		"column", c.Name,
		"table", qualifyMySQL(t),
		"detail", "the Dameng catalogue does not report IDENTITY(seed,increment), so the printed AUTO_INCREMENT starts at 1",
	)
	return at, nil
}

// integerWord reports whether a printed MySQL type can carry AUTO_INCREMENT.
func integerWord(word string) bool {
	switch word {
	case "tinyint", "smallint", "mediumint", "int", "bigint":
		return true
	}
	return false
}

// columnLine prints one column's definition clause.
func columnLine(c Column, autoInc bool) (string, error) {
	if c.Virtual {
		return "", errors.Errorf("column %s is generated from %s: a generation expression is Oracle grammar, and this build does not rewrite expressions",
			mysqlIdent(c.Name), strings.TrimSpace(c.Default))
	}
	if c.Hidden {
		return "", errors.Errorf("column %s is a hidden catalogue column, which a MySQL table has no equivalent for", mysqlIdent(c.Name))
	}
	if c.Mark != "" {
		return "", errors.Errorf("column %s carries catalogue mark %s, which this build has not measured on Dameng", mysqlIdent(c.Name), c.Mark)
	}
	typ, err := columnType(c)
	if err != nil {
		return "", err
	}
	parts := []string{mysqlIdent(c.Name), typ}
	// An auto-increment column is generated by the target, so its NOT NULL comes
	// from the target's own rule rather than from the source's flag: MySQL insists
	// on it, and Dameng's catalogue already says NOT NULL for an IDENTITY column.
	if autoInc {
		parts = append(parts, "NOT NULL", "AUTO_INCREMENT")
	} else if !c.Nullable {
		parts = append(parts, "NOT NULL")
	}
	def, err := defaultClause(c, typ)
	if err != nil {
		return "", err
	}
	if def != "" {
		parts = append(parts, def)
	}
	if c.Comment != "" {
		text, err := mysqlString(c.Comment)
		if err != nil {
			return "", errors.Wrapf(err, "column %s has a comment that cannot be printed", mysqlIdent(c.Name))
		}
		parts = append(parts, "COMMENT "+text)
	}
	return strings.Join(parts, " "), nil
}

// columnType prints a column's MySQL type word from the catalogue's own numbers.
func columnType(c Column) (string, error) {
	rule, ok := mysqlRules[c.Type]
	if !ok {
		return "", errors.Errorf("column %s has type %s, which this build has no MySQL spelling for", mysqlIdent(c.Name), c.Type)
	}
	switch rule.family {
	case famRefuse:
		return "", errors.Errorf("column %s has type %s: %s", mysqlIdent(c.Name), c.Type, rule.reason)

	case famInt, famFloat, famText, famLargeBinary:
		return rule.word, nil

	case famChar, famVarchar, famBinarySized:
		return sizedType(c, rule)

	case famDecimal:
		return decimalType(c)

	case famDatetime, famTime:
		fsp := int64(0)
		if c.Scale.Valid {
			fsp = c.Scale.Int64
		}
		if fsp < 0 || fsp > int64(fspMax) {
			return "", errors.Errorf("column %s declares %d fractional seconds, and MySQL's %s holds 0 to %d",
				mysqlIdent(c.Name), fsp, rule.word, fspMax)
		}
		if fsp == 0 {
			return rule.word, nil
		}
		return rule.word + "(" + strconv.FormatInt(fsp, 10) + ")", nil
	}
	return "", errors.Errorf("column %s has type %s, which this printer has no rule for", mysqlIdent(c.Name), c.Type)
}

// sizedType prints a character or byte type whose declared size has to fit the
// target's ceiling.
//
// A VARCHAR or VARBINARY past the ceiling becomes the large-object word, which
// holds everything the declared size could reach; a fixed CHAR does not, because
// MySQL pads a CHAR to its own declared length and a different length is a
// different value. That difference is why one oversized character column is a
// widening and the other is a refusal.
func sizedType(c Column, rule mysqlRule) (string, error) {
	size := c.CharLength
	if rule.family == famBinarySized {
		size = c.ByteLength
	}
	if size <= 0 {
		return "", errors.Errorf("column %s has type %s with no declared size, which this build cannot print as a MySQL %s",
			mysqlIdent(c.Name), c.Type, rule.word)
	}
	switch rule.family {
	case famChar:
		if size > int(charMax) {
			return "", errors.Errorf("column %s declares %s(%d) characters and MySQL's CHAR stops at %d: a longer fixed column is a different length, not a wider one",
				mysqlIdent(c.Name), rule.word, size, charMax)
		}
	case famVarchar:
		if size > int(varcharMax) {
			return "longtext", nil
		}
	case famBinarySized:
		if rule.word == "binary" {
			if size > int(charMax) {
				return "", errors.Errorf("column %s declares BINARY(%d) and MySQL's BINARY stops at %d",
					mysqlIdent(c.Name), size, charMax)
			}
		} else if size > int(varbinaryMax) {
			return "longblob", nil
		}
	}
	return rule.word + "(" + strconv.Itoa(size) + ")", nil
}

// decimalType prints an exact number. The questions go in this order on purpose:
// an unstated precision is refused first, because "any value the server can
// hold" is a fact about the values, and no MySQL exact type says that.
func decimalType(c Column) (string, error) {
	if !c.Precision.Valid || c.Precision.Int64 <= 0 {
		return "", errors.Errorf("column %s is a %s declared without a precision, which holds values past any MySQL exact type",
			mysqlIdent(c.Name), c.Type)
	}
	if !c.Scale.Valid {
		return "", errors.Errorf("column %s is a %s whose scale the catalogue did not report",
			mysqlIdent(c.Name), c.Type)
	}
	prec, scale := c.Precision.Int64, c.Scale.Int64
	if scale < 0 {
		return "", errors.Errorf("column %s declares scale %d, which MySQL reads as a shift to the left of the decimal point rather than as a fraction",
			mysqlIdent(c.Name), scale)
	}
	if scale > 0 {
		if prec > int64(decimalMax) || scale > int64(decimalScaleMax) || scale > prec {
			return "", errors.Errorf("column %s declares %s(%d,%d) and MySQL's DECIMAL holds at most %d digits with %d of them after the point",
				mysqlIdent(c.Name), c.Type, prec, scale, decimalMax, decimalScaleMax)
		}
		return "decimal(" + strconv.FormatInt(prec, 10) + "," + strconv.FormatInt(scale, 10) + ")", nil
	}
	// A scaleless number lands on an integer word by how many digits the
	// precision allows. The boundaries are the signed maxima rounded down to a
	// digit count, so a declared NUMBER(10,0) becomes INT rather than the
	// smallest type whose maximum happens to have ten digits: 2147483647 is ten
	// digits and 9999999999 is not, and only the first of the two fits.
	switch {
	case prec <= 3:
		return "tinyint", nil
	case prec <= 5:
		return "smallint", nil
	case prec <= 7:
		return "mediumint", nil
	case prec <= 9:
		return "int", nil
	case prec <= 18:
		return "bigint", nil
	case prec <= int64(decimalMax):
		return "decimal(" + strconv.FormatInt(prec, 10) + ",0)", nil
	}
	return "", errors.Errorf("column %s declares %s(%d,0), past MySQL's DECIMAL maximum of %d digits",
		mysqlIdent(c.Name), c.Type, prec, decimalMax)
}

// defaultClause prints DEFAULT, or refuses a default this build cannot restate.
//
// Three shapes pass: a number, a quoted string, and CURRENT_TIMESTAMP on a date
// or time column. The third is an allow-list rather than an expression rewrite —
// the spelling is identical in both dialects and the target evaluates it itself,
// which is why the type check is on the printed word and not on the text.
// Everything else, including a generation expression, `SYSDATE`, and a function
// call of any kind, stops the run: printing a Dameng expression unchanged would
// either fail on replay or, worse, mean something else on the target.
func defaultClause(c Column, typ string) (string, error) {
	if !c.HasDefault {
		return "", nil
	}
	text := strings.TrimSpace(c.Default)
	if text == "" {
		return "", nil
	}
	if strings.EqualFold(text, "NULL") {
		if !c.Nullable {
			return "", errors.Errorf("column %s is NOT NULL and defaults to NULL, which MySQL does not accept", mysqlIdent(c.Name))
		}
		return "DEFAULT NULL", nil
	}
	if numericDefault.MatchString(text) || stringDefault.MatchString(text) {
		return "DEFAULT " + text, nil
	}
	if timestampDefault.MatchString(text) {
		if !strings.HasPrefix(typ, "datetime") && !strings.HasPrefix(typ, "time") {
			return "", errors.Errorf("column %s defaults to CURRENT_TIMESTAMP, which MySQL evaluates only for a date or time column; the column prints as %s",
				mysqlIdent(c.Name), typ)
		}
		return "DEFAULT " + text, nil
	}
	return "", errors.Errorf("column %s has the default %s, which is not a literal this build prints: a Dameng expression default is Oracle grammar, and this build does not rewrite expressions",
		mysqlIdent(c.Name), text)
}

// keyLines prints the table's key clauses: the primary key, then the unique
// keys, then the rest, each in the name order the reader collected them in.
func keyLines(t *Table) ([]string, error) {
	var out []string
	if len(t.PrimaryKey) > 0 {
		cols, err := keyColumns(t, t.PrimaryKey)
		if err != nil {
			return nil, err
		}
		out = append(out, "PRIMARY KEY ("+strings.Join(cols, ", ")+")")
	}
	for _, k := range t.UniqueKeys {
		line, err := keyLine(t, k, "UNIQUE KEY ")
		if err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	for _, k := range t.Indexes {
		line, err := keyLine(t, k, "KEY ")
		if err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, nil
}

// keyLine prints one secondary key.
func keyLine(t *Table, k Key, verb string) (string, error) {
	if k.Expression {
		return "", errors.Errorf("index %s is not a plain column list: the catalogue reported an index type this build does not print, and MySQL has no matching spelling for it", mysqlIdent(k.Name))
	}
	if len(k.Columns) == 0 {
		return "", errors.Errorf("index %s reports no columns, which cannot be printed as a key", mysqlIdent(k.Name))
	}
	if len(k.Name) > int(keyNameMax) {
		return "", errors.Errorf("index %s is %d characters long and MySQL's limit for an index name is %d",
			k.Name, len(k.Name), keyNameMax)
	}
	cols, err := keyColumns(t, k.Columns)
	if err != nil {
		return "", err
	}
	return verb + mysqlIdent(k.Name) + " (" + strings.Join(cols, ", ") + ")", nil
}

// keyColumns prints a key's column list, refusing the two shapes the target
// cannot index: a column that is not in the table at all, and a column whose
// declared width passes InnoDB's key limit.
//
// The type of each key column is resolved here rather than trusted from the
// column loop, because the refusal has to name the same type word the printed
// statement uses, and a key over a column the printer refused would otherwise
// reach a message about a column the statement does not have.
func keyColumns(t *Table, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, errors.New("a key with no columns cannot be printed")
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		c := t.column(name)
		if c == nil {
			return nil, errors.Errorf("key column %s is not in the table's column list", upper(name))
		}
		word, err := columnType(*c)
		if err != nil {
			return nil, err
		}
		if wide := keyBytes(*c, word); wide > indexKeyMaxBytes {
			return nil, errors.Errorf("key column %s is %s, which needs %d bytes in a key and InnoDB allows %d: index it on a prefix instead",
				mysqlIdent(c.Name), word, wide, indexKeyMaxBytes)
		}
		out = append(out, mysqlIdent(c.Name))
	}
	return out, nil
}

// keyBytes estimates what one column contributes to an index key, in the bytes
// the target counts: four for a character, one for a byte string, the fixed
// width of a number, and a large object — which is the case the caller turns
// into a refusal, because MySQL needs a prefix length for it.
func keyBytes(c Column, word string) int {
	switch {
	case word == "longtext", word == "longblob", word == "text", word == "blob":
		return indexKeyMaxBytes + 1
	case strings.HasPrefix(word, "longtext"), strings.HasPrefix(word, "longblob"):
		return indexKeyMaxBytes + 1
	case strings.HasPrefix(word, "varchar"), strings.HasPrefix(word, "char"):
		return c.CharLength * utf8mb4Bytes
	case strings.HasPrefix(word, "varbinary"), strings.HasPrefix(word, "binary"):
		return c.ByteLength
	}
	return 0
}

// column finds a column by its stored name.
func (t *Table) column(name string) *Column {
	for i := range t.Columns {
		if strings.EqualFold(t.Columns[i].Name, name) {
			return &t.Columns[i]
		}
	}
	return nil
}

// mysqlIdent quotes one identifier the way MySQL's own SHOW CREATE writes them.
// A backquote inside a name doubles, which is the escape the grammar itself uses.
func mysqlIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// mysqlString prints one string literal.
//
// A backslash is doubled as well as a quote: MySQL's default parsing takes a
// backslash as an escape lead-in inside a literal, and printing the source text
// unchanged would let a comment ending in one swallow the closing quote.
func mysqlString(s string) (string, error) {
	if strings.ContainsRune(s, 0) {
		return "", errors.New("the text contains a NUL byte, which no MySQL string literal can hold")
	}
	escaped := strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s)
	return "'" + escaped + "'", nil
}
