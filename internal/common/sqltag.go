package common

import (
	"regexp"
	"strings"
)

// NoSQLTag disables the attribution comment for the whole run. It is set once
// from the --no-sql-tag persistent flag (or IMEX_NO_SQL_TAG) in the root
// command's PersistentPreRunE, before any subcommand executes a statement.
var NoSQLTag bool

// hexCommit matches a Git short hash. Restricting the tag to [0-9a-f] is what
// makes it impossible to break out of the surrounding block comment, so a
// malicious build-time value cannot inject SQL.
var hexCommit = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// sqlTag caches the rendered prefix. It depends only on GitCommit (injected at
// link time) and NoSQLTag (set once before the first statement), so computing
// it per call would waste allocations on every batched INSERT of a parallel
// import. ResetSQLTag must therefore be called whenever either changes.
var (
	sqlTagOnce bool
	sqlTagVal  string
)

// SQLTag returns the self-describing block comment prepended to every statement
// the tool executes itself, e.g. "/* imex:9183226 */". The commit lets
// general_log, slow_query_log and audit streams be filtered by tool origin, and
// identifies the exact build that produced a given statement.
//
// It returns "" when tagging is disabled, when the build did not inject a
// commit (dev builds report "unknown"), or when the injected value is not a
// plausible hexadecimal hash.
func SQLTag() string {
	if sqlTagOnce {
		return sqlTagVal
	}
	sqlTagOnce = true
	sqlTagVal = buildSQLTag()
	return sqlTagVal
}

func buildSQLTag() string {
	if NoSQLTag {
		return ""
	}
	c := strings.ToLower(strings.TrimSpace(GitCommit))
	if !hexCommit.MatchString(c) {
		return ""
	}
	if len(c) > 7 {
		c = c[:7]
	}
	return "/* imex:" + c + " */"
}

// ResetSQLTag drops the cached tag. Callers that change GitCommit or NoSQLTag
// after the first statement (only tests do this) must invalidate the cache.
func ResetSQLTag() {
	sqlTagOnce = false
	sqlTagVal = ""
}

// TagSQL prefixes a statement with the attribution comment. Statements the tool
// does not execute itself (SET NAMES issued by the DSN, transaction control such
// as START TRANSACTION / COMMIT / ROLLBACK, and SET FOREIGN_KEY_CHECKS) must NOT
// be wrapped: they are connection-lifecycle noise, not data operations worth
// auditing.
//
// A leading block comment does not interfere with an OceanBase/MySQL optimiser
// hint in the user's --query, because hints are recognised as "/*+ ... */" at
// the start of the statement text; the tag is a plain "/* ... */" comment.
func TagSQL(sql string) string {
	tag := SQLTag()
	if tag == "" {
		return sql
	}
	return tag + " " + sql
}
