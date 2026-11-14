package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
	"github.com/imex/internal/show"
)

func validShowCfg() *config.ShowConfig {
	cfg := config.NewShowConfig()
	cfg.Password = "pass"
	cfg.Database = "mydb"
	cfg.Object = "users"
	return cfg
}

func TestShowKindNamesAreUnique(t *testing.T) {
	seen := map[string]string{}
	for _, k := range show.Kinds() {
		for _, name := range []string{k.Short, k.Full} {
			if other, dup := seen[name]; dup {
				t.Errorf("name %q claimed by %s and %s", name, other, k.Short)
			}
			seen[name] = k.Short
		}
	}
	// `show evt` and `show tab` must not collide with each other either.
	if len(seen) != 2*len(show.Kinds()) {
		t.Errorf("names = %d, want %d", len(seen), 2*len(show.Kinds()))
	}
}

func TestShowCommandTree(t *testing.T) {
	cmd := newShowCmd()
	if len(cmd.Commands()) != len(show.Kinds()) {
		t.Fatalf("subcommands = %d, want %d", len(cmd.Commands()), len(show.Kinds()))
	}
	for _, k := range show.Kinds() {
		for _, name := range []string{k.Short, k.Full} {
			found, _, err := cmd.Find([]string{name})
			if err != nil {
				t.Errorf("%q: %v", name, err)
				continue
			}
			// Cobra reports the registered Use, which is the three-letter name; the
			// English word only works as an alias.
			if found.Name() != k.Short {
				t.Errorf("%q resolved to %q, want %q", name, found.Name(), k.Short)
			}
		}
	}
}

func TestShowKindCommandsRegisterTheChannelFlags(t *testing.T) {
	for _, k := range show.Kinds() {
		for _, name := range []string{k.Short, k.Full} {
			cmd := newShowCmd()
			found, _, err := cmd.Find([]string{name})
			if err != nil {
				t.Fatalf("%q: %v", name, err)
			}
			for flagName, shorthand := range map[string]string{
				"output":       "o",
				"no-overwrite": "",
				"messages":     "m",
				"database":     "D",
				"help":         "?",
			} {
				flag := found.Flags().Lookup(flagName)
				if flag == nil {
					t.Errorf("%s: --%s missing", name, flagName)
					continue
				}
				if flag.Shorthand != shorthand {
					t.Errorf("%s: --%s shorthand = %q, want %q", name, flagName, flag.Shorthand, shorthand)
				}
			}
			// -h belongs to --host on every leaf that talks to a server, so help is ?.
			if flag := found.Flags().Lookup("host"); flag == nil || flag.Shorthand != "h" {
				t.Errorf("%s: -h must stay --host", name)
			}
			// Enumeration is `list`'s job; show takes exactly one object name.
			if flag := found.Flags().Lookup("pattern"); flag != nil {
				t.Errorf("%s: --pattern must not exist on show", name)
			}
		}
	}
}

func TestShowMessagesFlagTextIsSharedWithList(t *testing.T) {
	// One wording for the channel model across both commands.
	showFlags := newShowCmd().Commands()[0].Flags()
	listFlags := newListCmd().Commands()[0].Flags()
	for _, name := range []string{"output", "messages", "no-overwrite"} {
		a, b := showFlags.Lookup(name), listFlags.Lookup(name)
		if a == nil || b == nil {
			t.Fatalf("%s: show=%v list=%v", name, a != nil, b != nil)
		}
		if a.Usage != b.Usage {
			t.Errorf("%s usage differs:\n show: %q\n list: %q", name, a.Usage, b.Usage)
		}
	}
}

func TestShowWithoutKindIsUsageError(t *testing.T) {
	cmd := newShowCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil || !common.IsUsageError(err) {
		t.Fatalf("got %v, want a usage error", err)
	}
	want := "show needs an object subcommand: " + strings.Join(show.ShortNames(), ", ")
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestShowUnknownKindIsUsageError(t *testing.T) {
	cmd := newShowCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"tabel"})
	err := cmd.Execute()
	if err == nil || !common.IsUsageError(err) {
		t.Fatalf("got %v, want a usage error", err)
	}
	if !strings.Contains(err.Error(), `unknown object subcommand "tabel"`) {
		t.Errorf("error = %v", err)
	}
	// The message must list the names a user can type.
	for _, short := range show.ShortNames() {
		if !strings.Contains(err.Error(), short) {
			t.Errorf("error omits kind %q: %v", short, err)
		}
	}
}

func TestShowArgumentCountIsEnforced(t *testing.T) {
	for _, args := range [][]string{
		{"tab"},
		{"tab", "users", "extra"},
	} {
		cmd := newShowCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		err := cmd.Execute()
		if err == nil || !common.IsUsageError(err) {
			t.Fatalf("%v: got %v, want a usage error", args, err)
		}
		if !strings.Contains(err.Error(), "accepts 1 argument(s)") {
			t.Errorf("%v: error = %v", args, err)
		}
	}
}

func TestValidateShowConfigAcceptsBothNameForms(t *testing.T) {
	cfg := validShowCfg()
	if err := validateShowConfig(cfg, show.Kind{Short: "tab", Full: "table"}); err != nil {
		t.Fatalf("unqualified: %v", err)
	}
	// A qualified name makes --database redundant.
	cfg.Database = ""
	cfg.Schema = "other"
	if err := validateShowConfig(cfg, show.Kind{Short: "tab", Full: "table"}); err != nil {
		t.Fatalf("qualified: %v", err)
	}
}

func TestValidateShowConfigRejectsMissingScope(t *testing.T) {
	cfg := validShowCfg()
	cfg.Database = ""
	err := validateShowConfig(cfg, show.Kind{Short: "tab", Full: "table"})
	if err == nil || !strings.Contains(err.Error(), "--database is required for show") {
		t.Fatalf("got %v", err)
	}
	if !common.IsUsageError(err) {
		t.Errorf("error must be a usage error, got %T", err)
	}
}

func TestValidateShowConfigRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		edit func(*config.ShowConfig)
		want string
	}{
		{"password", func(c *config.ShowConfig) { c.Password = "" }, "--password is required (or set IMEX_PASSWORD)"},
		{"user", func(c *config.ShowConfig) { c.User = "" }, "--user is required"},
		{"host", func(c *config.ShowConfig) { c.Host = "" }, "--host is required"},
		{"port", func(c *config.ShowConfig) { c.Port = 0 }, "invalid --port"},
		{"log level", func(c *config.ShowConfig) { c.LogLevel = "verbose" }, "invalid --log-level"},
		{"charset", func(c *config.ShowConfig) { c.DefaultCharset = "klingon" }, "invalid --default-charset"},
		{"connect timeout", func(c *config.ShowConfig) { c.ConnectTimeout = 0 }, "invalid --connect-timeout"},
		{"read timeout", func(c *config.ShowConfig) { c.SocketReadTimeout = -1 }, "invalid --socket-read-timeout"},
		{"write timeout", func(c *config.ShowConfig) { c.SocketWriteTimeout = -1 }, "invalid --socket-write-timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validShowCfg()
			tc.edit(cfg)
			err := validateShowConfig(cfg, show.Kind{Short: "tab", Full: "table"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want substring %q", err, tc.want)
			}
			if !common.IsUsageError(err) {
				t.Fatalf("error must be a usage error, got %T", err)
			}
		})
	}
}

func TestValidateShowConfigRefusesResultAndMessageSameFile(t *testing.T) {
	prev := messagesPath
	messagesPath = "/tmp/same.log"
	defer func() { messagesPath = prev }()

	cfg := validShowCfg()
	cfg.Output = "/tmp/same.log"
	err := validateShowConfig(cfg, show.Kind{Short: "tab", Full: "table"})
	if err == nil || !strings.Contains(err.Error(), "--output and --messages must name different files") {
		t.Fatalf("got %v", err)
	}
	if !common.IsUsageError(err) {
		t.Errorf("error must be a usage error, got %T", err)
	}

	cfg.Output = "/tmp/other.log"
	if err := validateShowConfig(cfg, show.Kind{Short: "tab", Full: "table"}); err != nil {
		t.Errorf("distinct files refused: %v", err)
	}
}

func TestShowHelpLinesStayWithinTheLimit(t *testing.T) {
	kinds := show.Kinds()
	surfaces := [][]string{{"--help"}}
	for _, k := range kinds {
		surfaces = append(surfaces, []string{k.Short, "--help"})
	}
	for _, args := range surfaces {
		cmd := newShowCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for _, line := range strings.Split(out.String(), "\n") {
			if len(line) > 118 {
				t.Errorf("%v: line is %d bytes: %q", args, len(line), line)
			}
		}
	}
}

// TestValidateShowConfigAcceptsSchemaWithoutDatabase pins the one kind that needs
// no scope option: a schema is the name itself, so `imex show sch appdb` must
// validate with no --database. The same config for a table is still refused,
// which is what keeps the relaxation from leaking to the other kinds.
func TestValidateShowConfigAcceptsSchemaWithoutDatabase(t *testing.T) {
	cfg := validShowCfg()
	cfg.Database = ""
	cfg.Schema = ""
	cfg.Object = "appdb"

	if err := validateShowConfig(cfg, show.Kind{Short: "sch", Full: "schema"}); err != nil {
		t.Fatalf("show schema without --database: %v", err)
	}
	err := validateShowConfig(cfg, show.Kind{Short: "tab", Full: "table"})
	if err == nil || !strings.Contains(err.Error(), "--database is required for show") {
		t.Fatalf("show table without scope = %v, want the --database refusal", err)
	}
}
