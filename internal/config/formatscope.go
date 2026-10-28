package config

// The --format values. Named because three packages now have to agree on them:
// the writers, the readers, and the format-scope registry below.
const (
	FormatDel     = "del"
	FormatSQL     = "sql"
	FormatParquet = "parquet"
)

// OptionScope registers one format-scoped flag: which formats read it, the marker
// its help text carries, and the message a mismatch is reported with.
//
// This is the single registry of format applicability, and three consumers read
// it so they cannot drift apart:
//
//   - cmd/validate.go rejects a scoped flag the user explicitly passed together
//     with a format that does not use it (exit code 2),
//   - the `export config` / `import config` record prints only the flags the
//     chosen format actually reads, which the rejection above guarantees are the
//     ones with a meaningful value,
//   - cmd appends Hint to the flag's help text, so `--help` says the same thing
//     the validator enforces.
//
// Adding a format-scoped option therefore means one entry here.
type OptionScope struct {
	// Flag is the command-line name, without the leading "--".
	Flag string

	// Formats lists the file formats that read this flag.
	Formats []string

	// Hint is appended to the flag's help text.
	Hint string

	// Reject renders the usage error for a flag given with an unusable format.
	// The wording differs per group on purpose: the SQL and DEL modifiers read
	// one writer each ("can only be used with --format X"), while the binary
	// encoding flags are legal for two formats and only refused by parquet
	// ("cannot be used with --format parquet: ...").
	Reject func(format string) string
}

// OptionScopes is one subcommand's registry, in the order mismatches are
// reported in.
type OptionScopes []OptionScope

// onlyWith builds a scope that one format reads and reports the mismatch by
// naming that format.
func onlyWith(flag, format, hintSuffix string) OptionScope {
	return OptionScope{
		Flag:    flag,
		Formats: []string{format},
		Hint:    "(only with " + hintSuffix + ")",
		Reject: func(string) string {
			return "--" + flag + " can only be used with --format " + format
		},
	}
}

// notWith builds a scope that every format but one reads.
func notWith(flag string, formats []string, refused, reason string) OptionScope {
	return OptionScope{
		Flag:    flag,
		Formats: formats,
		Hint:    "(not with --format " + refused + ")",
		Reject: func(format string) string {
			return "--" + flag + " cannot be used with --format " + format + ": " + reason
		},
	}
}

// delOnly is the DEL modifier group: they describe how a delimited file is laid
// out or encoded, so the SQL and Parquet writers have no use for them, and
// silently ignoring them would let a user believe the file was written another
// way.
func delOnly(flag string) OptionScope { return onlyWith(flag, FormatDel, "--format "+FormatDel) }

// sqlOnly is the SQL layout group, for the options that only the SQL writer
// reads.
func sqlOnly(flag string) OptionScope { return onlyWith(flag, FormatSQL, "--format "+FormatSQL) }

// parquetOnly is the Parquet writer group: options that describe how a columnar
// file is laid out on disk have no meaning for the row-oriented writers.
func parquetOnly(flag string) OptionScope {
	return onlyWith(flag, FormatParquet, "--format "+FormatParquet)
}

// binaryEncodingReason is shared by the two hex-blob flags.
const binaryEncodingReason = "binary columns are already stored as binary"

// ExportOptionScopes lists the export flags that are not format-independent.
// The order is the order mismatches are reported in, and it is what the shipped
// CLI has always produced: SQL layout first, then DEL modifiers, then the binary
// encoding pair.
func ExportOptionScopes() OptionScopes {
	delAndSQL := []string{FormatDel, FormatSQL}
	return OptionScopes{
		sqlOnly("create"),
		sqlOnly("no-data"),
		sqlOnly("statement-size"),
		sqlOnly("extended-insert"),
		parquetOnly("parquet-compression"),
		delOnly("char-del"),
		delOnly("col-del"),
		delOnly("row-del"),
		delOnly("escape"),
		delOnly("no-escape"),
		delOnly("timestamp-fmt"),
		delOnly("date-fmt"),
		delOnly("no-char-del"),
		delOnly("no-col-names"),
		notWith("hex-blob", delAndSQL, FormatParquet, binaryEncodingReason),
		notWith("no-hex-blob", delAndSQL, FormatParquet, binaryEncodingReason),
	}
}

// ImportOptionScopes lists the import flags that are not format-independent.
// Only the DEL modifier group qualifies: --statement-size on this side bounds the
// batches the importer builds whatever the input file looks like, and --mode has
// its own value-level check in validateMode.
func ImportOptionScopes() OptionScopes {
	return OptionScopes{
		delOnly("char-del"),
		delOnly("col-del"),
		delOnly("row-del"),
		delOnly("escape"),
		delOnly("no-escape"),
		delOnly("timestamp-fmt"),
		delOnly("date-fmt"),
		delOnly("no-char-del"),
		delOnly("no-col-names"),
	}
}

// AppliesTo reports whether a flag has any effect for the chosen format. A flag
// that is not registered at all is format-independent, so it applies.
func (s OptionScopes) AppliesTo(flag, format string) bool {
	for _, sc := range s {
		if sc.Flag != flag {
			continue
		}
		for _, f := range sc.Formats {
			if f == format {
				return true
			}
		}
		return false
	}
	return true
}

// Violation walks the registry in order and returns the message for the first
// flag that `changed` reports as explicitly given but that the chosen format does
// not read. It reports false when every given flag applies.
//
// changed is cobra's Flags().Changed: a value the user never touched is a default
// and defaults are harmless, which is exactly why the check is about what was
// passed rather than what the fields hold.
func (s OptionScopes) Violation(changed func(string) bool, format string) (string, bool) {
	for _, sc := range s {
		if changed(sc.Flag) && !s.AppliesTo(sc.Flag, format) {
			return sc.Reject(format), true
		}
	}
	return "", false
}
