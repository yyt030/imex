package backend

import (
	"fmt"

	"github.com/go-sql-driver/mysql"

	"github.com/imex/internal/config"
)

// reservedMySQLDSNParams are DSN parameter names that the tool itself sets or that
// the go-sql-driver treats specially (they configure the driver/connection
// rather than a MySQL session variable, and "strict" even panics on parse).
// A session variable using one of these names would be silently hijacked at
// DSN re-parse time, so it is rejected up front.
//
// The list moved here from internal/common with the DSN builder, because every
// name on it is either one buildMySQLDSN writes or one this driver's parser
// reads. An engine that has no DSN parameters has no reserved names, and says so
// (see postgresBackend.IsReservedConnParam).
var reservedMySQLDSNParams = map[string]bool{
	// set by the tool (see buildMySQLDSN)
	"charset":           true,
	"parseTime":         true,
	"interpolateParams": true,
	"multiStatements":   true,
	"clientFoundRows":   true,
	"tls":               true,
	"client-cert":       true,
	"client-key":        true,
	"ca-cert":           true,
	"tenant":            true,
	// interpreted by the driver's DSN parser
	"allowAllFiles":            true,
	"allowCleartextPasswords":  true,
	"allowFallbackToPlaintext": true,
	"allowNativePasswords":     true,
	"allowOldPasswords":        true,
	"checkConnLiveness":        true,
	"collation":                true,
	"columnsWithAlias":         true,
	"compress":                 true,
	"connectionAttributes":     true,
	"loc":                      true,
	"maxAllowedPacket":         true,
	"readTimeout":              true,
	"rejectReadOnly":           true,
	"serverPubKey":             true,
	"strict":                   true,
	"timeTruncate":             true,
	"timeout":                  true,
	"writeTimeout":             true,
}

// IsReservedConnParam answers the --session-var name check: a name the tool or the
// driver already owns cannot be overridden from the command line.
func (mysqlBackend) IsReservedConnParam(name string) bool {
	return reservedMySQLDSNParams[name]
}

// buildMySQLDSN renders the connection string for a Config. It uses mysql.Config so
// special characters in the username and password are escaped rather than
// reinterpreted as DSN syntax.
//
// OceanBase usernames are passed through verbatim; the tenant and cluster are
// part of the user string (`root@sys`, `root@sys#obcluster`), not separate
// options, which is why the removed --tenant flag has no replacement here.
func buildMySQLDSN(c *config.Config) string {
	cfg := mysql.NewConfig()
	cfg.User = c.User
	cfg.Passwd = c.Password
	cfg.DBName = c.Database
	cfg.Params = make(map[string]string)
	cfg.Params["charset"] = c.DefaultCharset
	// parseTime is intentionally disabled: the exporter reads every column as
	// sql.RawBytes (the exact text stored by the server). If parseTime were
	// enabled, DATE/DATETIME/TIMESTAMP columns would be returned as Go
	// time.Time and rendered as RFC3339 ("2026-01-01T10:00:00Z"), which is not
	// a valid MySQL DATETIME literal and breaks round-tripping.
	cfg.Params["parseTime"] = "false"
	cfg.Params["interpolateParams"] = "true"
	cfg.Params["multiStatements"] = "false"
	cfg.Params["clientFoundRows"] = "false"

	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%d", c.Host, c.Port)

	// Timeouts are expressed with the driver's own fields so they survive
	// FormatDSN/ParseDSN round-tripping. connectTimeout is a dial deadline;
	// readTimeout/writeTimeout bound a single socket read or write, which is
	// what actually stops a stalled server from hanging a long export.
	cfg.Timeout = c.ConnectTimeout
	cfg.ReadTimeout = c.SocketReadTimeout
	cfg.WriteTimeout = c.SocketWriteTimeout

	// --session-var values travel as extra DSN parameters. The driver executes
	// "SET <name> = <value>" for every parameter it does not recognise, on every
	// NEW physical connection (connector.go -> handleParams). That is the only
	// mechanism that reliably survives database/sql pooling: the main export
	// connection, every shard worker and every parallel import worker carry the
	// variables without extra lifecycle hooks.
	for name, value := range c.Vars {
		if reservedMySQLDSNParams[name] {
			// Defensive: ParseSessionVars already rejects these.
			continue
		}
		cfg.Params[name] = value
	}

	return cfg.FormatDSN()
}
