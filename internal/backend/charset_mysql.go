package backend

import (
	"strings"
)

// mysqlCharsets is the full set of values --default-charset accepts on MySQL,
// aliases included. It is the vocabulary SET NAMES understands on this engine,
// so it belongs next to the engine rather than in cmd/: a future dialect that
// names charsets differently brings its own set, and the command-side format
// rules (parquet only utf8mb4, SQL only byte-escape-safe names — those are about
// the output file, not the server) stay engine-neutral.
//
// Moved here from cmd/validate.go in round 134. The accepted names, aliases and
// the rejection sentence are unchanged.
var mysqlCharsets = map[string]bool{
	"utf8mb4": true, "utf8": true, "utf-8": true, "utf8mb3": true,
	"ascii": true, "us-ascii": true, "384": true,
	"latin1": true, "iso-8859-1": true, "819": true,
	"gbk": true, "936": true, "1386": true,
	"gb18030": true, "5488": true, "1392": true,
}

// mysqlCharsetName normalises a charset for lookup the same way the command used
// to: trim surrounding space, fold case. The original spelling is what appears in
// the error message.
func mysqlCharsetName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// KnownCharset reports whether name is one of the 16 values above. cmd's generic
// "unrecognized charset" gate calls this at the same point in validation it always
// has — after the required-connection checks — so a run with both a bad charset and
// a missing password still reports the password first, exactly as before the move.
func (mysqlBackend) KnownCharset(name string) bool {
	return mysqlCharsets[mysqlCharsetName(name)]
}

// ValidateCharset is nil for every charset: SET NAMES takes the server's own
// spelling, and the server is the one that reports a name it does not have. The
// tool-side vocabulary check is KnownCharset above, which owns the rejection
// sentence cmd prints.
func (mysqlBackend) ValidateCharset(string) error { return nil }
