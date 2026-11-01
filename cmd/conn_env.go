package cmd

import (
	"os"
	"strconv"
	"strings"

	"github.com/spf13/pflag"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// connEnvSources pairs each connection flag with the environment variable that
// can supply its value. The order is the order the resolved variables are
// reported in the log record.
var connEnvSources = []struct {
	flag string
	env  string
}{
	{"host", "IMEX_HOST"},
	{"port", "IMEX_PORT"},
	{"user", "IMEX_USER"},
	{"password", "IMEX_PASSWORD"},
	{"socket", "IMEX_SOCKET"},
	{"database", "IMEX_DATABASE"},
}

// connEnvReport says which environment variables took effect and which were
// deliberately ignored. It is returned rather than logged in place because the
// logger is only initialised after parameter validation, so a value that fails
// validation must be reportable without needing a live logger.
type connEnvReport struct {
	used    []string
	skipped []string
}

// applyConnEnv fills the connection fields from the environment.
//
// Precedence is explicit flag > environment > flag default. "Explicit" is
// decided by FlagSet.Changed, never by comparing the value against the default:
// a user who types `-u root` while IMEX_USER=gd is in the shell has stated an
// intention that the environment must not overwrite, even though root is also
// the built-in default. Inferring "equals the default, so it was not asked for"
// would let a stale environment silently redirect a run to another account —
// the worst possible failure for a tool that writes to the database.
//
// This is the opposite of the four IMEX_* behaviour switches (IMEX_LOG_FORMAT,
// IMEX_ERROR_STACK, IMEX_ECHO_SQL_VALUES, IMEX_NO_SQL_TAG), where the
// environment overrides the flag. The split is deliberate: a switch changes how
// a run is reported, while connection values decide which server is touched.
//
// A variable set to an empty or whitespace-only value counts as unset, matching
// how IMEX_LOG_FORMAT is read. IMEX_PORT is range-checked here so a bad value
// names the variable that carried it instead of surfacing as "invalid --port".
//
// The transport is decided once rather than field by field. common.BuildDSN
// selects the UNIX socket whenever Socket is non-empty, so an IMEX_SOCKET left
// in a shell profile would beat an explicit -h/-P and connect somewhere else.
// Therefore IMEX_SOCKET is ignored when -h or -P was given explicitly, and the
// ignore is reported. When neither was given, IMEX_SOCKET still wins over
// IMEX_HOST, which is the same preference the socket field has always had.
func applyConnEnv(f *pflag.FlagSet, c *config.Config) (connEnvReport, error) {
	var rep connEnvReport
	explicitTCP := f.Changed("host") || f.Changed("port")

	for _, src := range connEnvSources {
		value := strings.TrimSpace(os.Getenv(src.env))

		if src.flag == "socket" && value != "" && explicitTCP {
			// The run named an address, so it means TCP. Keep the field empty and
			// tell the user why their variable did not take effect.
			rep.skipped = append(rep.skipped, src.env)
			continue
		}
		if value == "" || f.Changed(src.flag) {
			continue
		}

		switch src.flag {
		case "host":
			c.Host = value
		case "port":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 65535 {
				return rep, common.Usagef("invalid IMEX_PORT %q: must be a port number between 1 and 65535", value)
			}
			c.Port = n
		case "user":
			c.User = value
		case "password":
			c.Password = value
		case "socket":
			c.Socket = value
		case "database":
			c.Database = value
		}
		rep.used = append(rep.used, src.env)
	}
	return rep, nil
}

// logConnEnv reports the outcome of applyConnEnv once the logger exists. Only
// variable names are ever printed: an environment value can be a password, and
// IMEX_PASSWORD may hold an ENC: ciphertext whose plaintext must stay private.
// The messages keep the repo's lowercase "<object> <event>" vocabulary, so the
// variable name is an attribute rather than the first word of the message.
func logConnEnv(rep connEnvReport) {
	for _, name := range rep.skipped {
		common.Logger.Warn("connection variable ignored",
			"variable", name,
			"reason", "this run names an address with --host or --port, so it stays on TCP")
	}
	if len(rep.used) > 0 {
		common.Logger.Info("connection values supplied by environment", "variables", rep.used)
	}
}
