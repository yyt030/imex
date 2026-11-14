package cmd

import (
	"strings"

	"github.com/spf13/pflag"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/config"
)

// This file is the single registration point for the connection, timeout, log
// and output-channel options shared by export, import, list, show and exec.
// Before it existed those ~13 flags were declared five times, once in each
// register*Flags, and the copies had already drifted: --engine's enumeration
// was spelled out five times, --default-charset was bound to a temp variable on
// export/import (to hide its default from help) and straight to the config on
// the other three, and --messages carried two different descriptions.
//
// connFlagOpts keeps every one of those differences explicit and visible. It is
// a struct of wording choices and binding targets, not of behaviour: the point
// of the refactor is that one declaration serves five commands while each
// command's --help stays byte-for-byte what it was. Any new connection option
// now has exactly one place to be added.

type connFlagOpts struct {
	// HelpLabel is the -?/--help flag's description, which names the command.
	HelpLabel string

	// DatabaseUsage differs because --database scopes different things: a
	// database on export/import, a catalogue filter on list, an object schema on
	// show, and a default schema on exec.
	DatabaseUsage string

	// CharsetTarget / CharsetDefault / CharsetUsage keep the two existing
	// shapes: export and import bind a temp variable with an empty default so
	// help shows no "(default ...)", while list/show/exec bind the config field
	// whose default is utf8mb4 and say so.
	CharsetTarget  *string
	CharsetDefault string
	CharsetUsage   string

	// MessagesUsage is the one flag whose description was not shared: the
	// data commands say nothing about a result channel, the metadata commands
	// spell out that the result still goes to stdout or --output.
	MessagesUsage string

	// OutputTarget / NoOverwriteTarget are nil on import, which has an input
	// file instead of an output file. OutputUsage differs for the same reason.
	OutputTarget      *string
	NoOverwriteTarget *bool
	OutputUsage       string
}

// engineUsage builds --engine's help text from the backend's own registry, so
// the value list cannot drift away from Parse or from the invalid-value error.
// The rendered string is unchanged: "Database engine: mysql, pg, dm".
func engineUsage() string {
	return "Database engine: " + strings.Join(backend.Engines(), ", ")
}

// registerConnFlags declares the shared option set on f, bound to cfg. Import
// passes no output targets and therefore gets no --output/--no-overwrite.
func registerConnFlags(f *pflag.FlagSet, cfg *config.Config, o connFlagOpts) {
	// Every command that owns a connection uses -h for --host, so help is
	// re-registered under -?. The parent list/show commands keep cobra's -h.
	f.BoolP("help", "?", false, o.HelpLabel)

	f.StringVar(&cfg.Engine, "engine", "mysql", engineUsage())
	f.StringVarP(&cfg.Host, "host", "h", "127.0.0.1", "Database host address (also IMEX_HOST)")
	f.IntVarP(&cfg.Port, "port", "P", 3306, "TCP/IP port (also IMEX_PORT)")
	f.StringVarP(&cfg.User, "user", "u", "root", "Database username (also IMEX_USER)")
	f.StringVarP(&cfg.Password, "password", "p", "", "Database password (also IMEX_PASSWORD)")
	f.StringVarP(&cfg.Database, "database", "D", "", o.DatabaseUsage)

	f.DurationVar(&cfg.ConnectTimeout, "connect-timeout", config.DefaultConnectTimeout, "TCP connect timeout")
	f.DurationVar(&cfg.SocketReadTimeout, "socket-read-timeout", config.DefaultSocketTimeout, "Per-read socket timeout")
	f.DurationVar(&cfg.SocketWriteTimeout, "socket-write-timeout", config.DefaultSocketTimeout, "Per-write socket timeout")

	f.StringVar(o.CharsetTarget, "default-charset", o.CharsetDefault, o.CharsetUsage)
	f.StringVarP(&messagesPath, "messages", "m", "", o.MessagesUsage)
	f.StringVar((*string)(&cfg.LogLevel), "log-level", "info",
		"Log level ("+enumNames(config.LogLevels())+")")

	if o.OutputTarget != nil {
		f.StringVarP(o.OutputTarget, "output", "o", "", o.OutputUsage)
		f.BoolVar(o.NoOverwriteTarget, "no-overwrite", false,
			"Refuse to overwrite an existing output file (default: overwrite)")
	}
}
