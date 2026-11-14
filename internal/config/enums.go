package config

import "slices"

// This file is the single source of truth for the option values the CLI accepts.
// The constants in config.go remain the vocabulary; these slices fix the ORDER
// in which a group is listed, because help text and invalid-value errors are
// part of the CLI's word-for-word contract. Before this existed, each value was
// spelled out three times: in the constants, in the validator's switch, and in
// the error/help strings, so adding a value meant three edits and a drift risk.
//
// Every slice below is deliberately in the order the existing messages print.

// Formats returns the --format values, in order.
func Formats() []string {
	return []string{FormatDel, FormatSQL, FormatParquet}
}

// ImportModes returns the --mode values, in order.
func ImportModes() []string {
	return []string{
		string(ImportModeInsert),
		string(ImportModeInsertUpdate),
		string(ImportModeReplace),
	}
}

// SQLDialects returns the --sql-dialect values, in order.
func SQLDialects() []string {
	return []string{
		string(SQLDialectAuto),
		string(SQLDialectMySQL),
		string(SQLDialectPG),
		string(SQLDialectDM),
	}
}

// ParquetCompressions returns the --parquet-compression values, in order.
func ParquetCompressions() []string {
	return []string{
		ParquetCompressionNone,
		ParquetCompressionSnappy,
		ParquetCompressionZstd,
	}
}

// LogLevels returns the --log-level values, in order.
func LogLevels() []string {
	return []string{
		string(LogLevelDebug),
		string(LogLevelInfo),
		string(LogLevelWarn),
		string(LogLevelError),
	}
}

// BlobEncodings returns the --blob-encoding values, in order. auto comes first
// because it is the default and the option's own help lists it first.
func BlobEncodings() []string {
	return []string{
		string(BlobEncodingAuto),
		string(BlobEncodingHex),
		string(BlobEncodingRaw),
	}
}

// HeaderModes returns the --header values, in order. on comes first because it
// is the default and the option's own help lists it first.
func HeaderModes() []string {
	return []string{string(HeaderOn), string(HeaderOff)}
}

// ValidHeaderMode reports whether s is a --header value.
func ValidHeaderMode(s string) bool { return slices.Contains(HeaderModes(), s) }

// HeaderEnabled resolves --header to the bool the writers read. An empty value
// (a caller building a config by hand) behaves as on, like the shipped default.
func HeaderEnabled(s string) bool { return HeaderMode(s) != HeaderOff }

// ValidFormat reports whether s is a --format value.
func ValidFormat(s string) bool { return slices.Contains(Formats(), s) }

// ValidImportMode reports whether s is a --mode value.
func ValidImportMode(s string) bool { return slices.Contains(ImportModes(), s) }

// ValidLogLevel reports whether s is a --log-level value. The caller is
// responsible for the trim/lower normalisation the option has always applied.
func ValidLogLevel(s string) bool { return slices.Contains(LogLevels(), s) }

// ValidBlobEncoding reports whether s is a --blob-encoding value.
func ValidBlobEncoding(s string) bool { return slices.Contains(BlobEncodings(), s) }
