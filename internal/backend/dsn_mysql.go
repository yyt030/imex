package backend

import (
	"fmt"
	"net/url"
	"strings"

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

// driverDSN renders the portion of a MySQL DSN after the userinfo: protocol,
// address, database and every parameter the tool owns or forwards. It is used
// two ways — appended to a userinfo by buildMySQLDSN (for the golden tests), and
// handed to mysql.ParseDSN by buildMySQLConfig so the driver consumes its own
// parameters into typed fields before the raw User/Passwd are attached.
func driverDSN(c *config.Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tcp(%s:%d)/%s?", c.Host, c.Port, url.PathEscape(c.Database))
	// parseTime is intentionally false: the exporter reads every column as
	// sql.RawBytes (the exact text stored by the server). Parsing times into
	// time.Time would render them as RFC3339, which is not a valid MySQL
	// DATETIME literal and breaks round-tripping.
	params := []string{
		"charset=" + url.QueryEscape(c.DefaultCharset),
		"parseTime=false",
		"interpolateParams=true",
		"multiStatements=false",
		"clientFoundRows=false",
		fmt.Sprintf("timeout=%s", c.ConnectTimeout),
		fmt.Sprintf("readTimeout=%s", c.SocketReadTimeout),
		fmt.Sprintf("writeTimeout=%s", c.SocketWriteTimeout),
	}
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
		params = append(params, name+"="+url.QueryEscape(value))
	}
	b.WriteString(strings.Join(params, "&"))
	return b.String()
}

// buildMySQLConfig builds the driver Config for a direct connection. It parses
// a userinfo-free DSN so go-sql-driver consumes its own parameters (charset,
// parseTime, timeouts, session vars) into typed fields, then attaches User and
// Passwd verbatim as typed fields. Nothing re-parses them, so a username or
// password containing ':' or '@' survives intact — e.g. the legitimate user
// `test@SERVICE:attest` no longer splits into `test@SERVICE` plus a mangled
// password, the trap that FormatDSN + sql.Open falls into. OceanBase names
// (`root@sys`, `root@sys#obcluster`) benefit from the same path.
func buildMySQLConfig(c *config.Config) (*mysql.Config, error) {
	cfg, err := mysql.ParseDSN(driverDSN(c))
	if err != nil {
		return nil, err
	}
	cfg.User = c.User
	cfg.Passwd = c.Password
	return cfg, nil
}

// buildMySQLDSN renders the connection string for a Config, for tests and
// diagnostics only. Production connections use buildMySQLConfig with
// mysql.NewConnector so special characters in User/Passwd are not re-parsed;
// this string form is retained because the DSN golden tests pin it byte for
// byte and it is the shape operators see when they reconstruct a run.
func buildMySQLDSN(c *config.Config) string {
	cfg := mysql.NewConfig()
	cfg.User = c.User
	cfg.Passwd = c.Password
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%d", c.Host, c.Port)
	cfg.DBName = c.Database
	cfg.Params = make(map[string]string)
	cfg.Params["charset"] = c.DefaultCharset
	cfg.Params["parseTime"] = "false"
	cfg.Params["interpolateParams"] = "true"
	cfg.Params["multiStatements"] = "false"
	cfg.Params["clientFoundRows"] = "false"
	cfg.Timeout = c.ConnectTimeout
	cfg.ReadTimeout = c.SocketReadTimeout
	cfg.WriteTimeout = c.SocketWriteTimeout
	for name, value := range c.Vars {
		if reservedMySQLDSNParams[name] {
			continue
		}
		cfg.Params[name] = value
	}
	return cfg.FormatDSN()
}
