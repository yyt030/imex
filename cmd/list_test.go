package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
	"github.com/imex/internal/list"
)

func validListCfg(object string) *config.ListConfig {
	cfg := config.NewListConfig()
	cfg.Object = object
	cfg.Password = "pass"
	// --type is bound with a per-object default; mirror it here because these
	// tests build the config directly instead of parsing flags.
	switch object {
	case "tables":
		cfg.Type = "base"
	case "routines":
		cfg.Type = "all"
	}
	return cfg
}

func TestValidateListConfigAcceptsEveryObject(t *testing.T) {
	for _, object := range []string{"databases", "tables", "views", "triggers", "routines", "events"} {
		cfg := validListCfg(object)
		if err := validateListConfig(cfg); err != nil {
			t.Errorf("%s: %v", object, err)
		}
	}

	for _, object := range []string{"columns", "indexes"} {
		cfg := validListCfg(object)
		cfg.Database = "mydb"
		cfg.Table = "users"
		if err := validateListConfig(cfg); err != nil {
			t.Errorf("%s: %v", object, err)
		}
	}
}

func TestValidateListConfigRejectsMissingScope(t *testing.T) {
	for _, object := range []string{"columns", "indexes"} {
		cfg := validListCfg(object)
		err := validateListConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "--database is required for list "+object) {
			t.Errorf("%s without --database: %v", object, err)
		}
		if !common.IsUsageError(err) {
			t.Errorf("%s: error must be a usage error, got %T", object, err)
		}

		cfg.Database = "mydb"
		err = validateListConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "--table is required for list "+object) {
			t.Errorf("%s without --table: %v", object, err)
		}
		if !common.IsUsageError(err) {
			t.Errorf("%s: error must be a usage error, got %T", object, err)
		}
	}
}

func TestValidateListConfigRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		edit func(*config.ListConfig)
		want string
	}{
		{"password", func(c *config.ListConfig) { c.Password = "" }, "--password is required (or set IMEX_PASSWORD)"},
		{"user", func(c *config.ListConfig) { c.User = "" }, "--user is required"},
		{"host", func(c *config.ListConfig) { c.Host = "" }, "--host is required"},
		{"port", func(c *config.ListConfig) { c.Port = 0 }, "invalid --port"},
		{"log level", func(c *config.ListConfig) { c.LogLevel = "verbose" }, "invalid --log-level"},
		{"charset", func(c *config.ListConfig) { c.DefaultCharset = "klingon" }, "invalid --default-charset"},
		{"table type", func(c *config.ListConfig) { c.Type = "nonsense" }, "invalid --type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validListCfg("tables")
			tc.edit(cfg)
			err := validateListConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want substring %q", err, tc.want)
			}
			if !common.IsUsageError(err) {
				t.Fatalf("error must be a usage error, got %T", err)
			}
		})
	}
}

func TestValidateListConfigRoutineType(t *testing.T) {
	for _, typ := range []string{"all", "procedure", "function"} {
		cfg := validListCfg("routines")
		cfg.Type = typ
		if err := validateListConfig(cfg); err != nil {
			t.Errorf("type %q: %v", typ, err)
		}
	}
	cfg := validListCfg("routines")
	cfg.Type = "trigger"
	if err := validateListConfig(cfg); err == nil {
		t.Error("bad routine type accepted")
	}
}

func TestListCommandTree(t *testing.T) {
	cmd := newListCmd()
	// Aligned with `show`: the three-letter name is the canonical Use and the
	// long name is a registered alias, so both spellings resolve to one leaf.
	want := map[string]string{
		"databases": "dbs", "schemas": "sch", "tables": "tab", "views": "vws",
		"columns": "col", "indexes": "idx", "triggers": "trg", "routines": "rtn",
		"events": "evt",
	}
	if len(cmd.Commands()) != len(want) {
		t.Fatalf("subcommands = %d, want %d", len(cmd.Commands()), len(want))
	}
	for full, short := range want {
		byShort, _, e1 := cmd.Find([]string{short})
		byFull, _, e2 := cmd.Find([]string{full})
		if e1 != nil || e2 != nil {
			t.Errorf("%q / %q: find errors %v / %v", short, full, e1, e2)
			continue
		}
		if byShort != byFull {
			t.Errorf("%q and %q resolved to different commands", short, full)
		}
		if byShort.Name() != short {
			t.Errorf("%q canonical Name = %q, want %q", full, byShort.Name(), short)
		}
	}
}

func TestListCommandAliases(t *testing.T) {
	// After the flip the short name is canonical and the long name is the alias,
	// so each leaf must carry its long name in Aliases and print short names in
	// `imex list --help`, exactly the way `show` presents its kinds.
	want := map[string]string{
		"dbs": "databases", "sch": "schemas", "tab": "tables", "vws": "views",
		"col": "columns", "idx": "indexes", "trg": "triggers", "rtn": "routines",
		"evt": "events",
	}
	cmd := newListCmd()
	for short, full := range want {
		found, _, err := cmd.Find([]string{short})
		if err != nil {
			t.Errorf("short %q: %v", short, err)
			continue
		}
		if found.Name() != short {
			t.Errorf("short %q resolved to leaf %q, want canonical %q", short, found.Name(), short)
		}
		if !containsName(found.Aliases, full) {
			t.Errorf("leaf %q aliases = %v, want to include %q", short, found.Aliases, full)
		}
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestListHasNoFormatFlag(t *testing.T) {
	cmd := newListCmd()
	found, _, err := cmd.Find([]string{"databases"})
	if err != nil {
		t.Fatal(err)
	}
	if flag := found.Flags().Lookup("format"); flag != nil {
		t.Fatalf("--format must be gone, found %q", flag.Usage)
	}
	if err := found.ParseFlags([]string{"-f", "table"}); err == nil {
		t.Fatal("-f must be an unknown flag")
	}
}

func TestListHelpLinesStayWithinTheLimit(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"databases", "--help"},
		{"tables", "--help"},
		{"views", "--help"},
		{"columns", "--help"},
		{"indexes", "--help"},
		{"triggers", "--help"},
		{"routines", "--help"},
		{"events", "--help"},
	} {
		cmd := newListCmd()
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

func TestListWithoutObjectIsUsageError(t *testing.T) {
	cmd := newListCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil || !common.IsUsageError(err) {
		t.Fatalf("got %v, want a usage error", err)
	}
	if !strings.Contains(err.Error(), "needs an object subcommand: "+strings.Join(list.ShortNames(), ", ")) {
		t.Errorf("error = %v", err)
	}
}

func TestListUnknownObjectIsUsageError(t *testing.T) {
	cmd := newListCmd()
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
}

// TestConnConfigForKeepsNamespaceNamesOutOfTheDMDsn covers the two DM commands
// whose --database filters a catalogue row instead of selecting a schema. The
// server reads the DSN's schema parameter at login, so a value that is fine to
// filter by ("DAMENG" for the one database, a missing schema for `list schemas`
// which should answer 0 rows) would otherwise fail the connection with -2103.
// Every other object, and every other engine, keeps the value.
func TestConnConfigForKeepsNamespaceNamesOutOfTheDMDsn(t *testing.T) {
	cfg := validListCfg(list.ObjectDatabases)
	cfg.Database = "DAMENG"

	for _, object := range []string{list.ObjectDatabases, list.ObjectSchemas} {
		if got := connConfigFor(backend.EngineDM, object, cfg); got.Database != "" {
			t.Errorf("dm %s must not send --database to the DSN, got %q", object, got.Database)
		}
	}
	if cfg.Database != "DAMENG" {
		t.Fatalf("the filter value was consumed: %q", cfg.Database)
	}
	for _, object := range []string{list.ObjectTables, list.ObjectColumns, list.ObjectIndexes} {
		if got := connConfigFor(backend.EngineDM, object, cfg); got.Database != "DAMENG" {
			t.Errorf("dm %s must keep --database in the DSN, got %q", object, got.Database)
		}
	}
	for _, e := range []backend.Engine{backend.EngineMySQL, backend.EnginePG} {
		for _, object := range []string{list.ObjectDatabases, list.ObjectSchemas} {
			if got := connConfigFor(e, object, cfg); got.Database != "DAMENG" {
				t.Errorf("%s %s must keep --database in the DSN, got %q", e, object, got.Database)
			}
		}
	}
}
