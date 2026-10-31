package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
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
		{"password", func(c *config.ListConfig) { c.Password = "" }, "password is required"},
		{"user", func(c *config.ListConfig) { c.User = "" }, "--user is required"},
		{"host and socket", func(c *config.ListConfig) { c.Host = ""; c.Socket = "" }, "--host or --socket is required"},
		{"port", func(c *config.ListConfig) { c.Port = 0 }, "invalid --port"},
		{"format", func(c *config.ListConfig) { c.Format = "yaml" }, `invalid --format "yaml"`},
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
	want := []string{"databases", "tables", "views", "columns", "indexes", "triggers", "routines", "events"}
	if len(cmd.Commands()) != len(want) {
		t.Fatalf("subcommands = %d, want %d", len(cmd.Commands()), len(want))
	}
	for _, name := range want {
		found, _, err := cmd.Find([]string{name})
		if err != nil || found.Name() != name {
			t.Errorf("subcommand %q not found: cmd=%v err=%v", name, found, err)
		}
	}
}

func TestListCommandAliases(t *testing.T) {
	want := map[string]string{
		"dbs": "databases",
		"tab": "tables",
		"vws": "views",
		"col": "columns",
		"idx": "indexes",
		"trg": "triggers",
		"rtn": "routines",
		"evt": "events",
	}
	cmd := newListCmd()
	for alias, object := range want {
		found, _, err := cmd.Find([]string{alias})
		if err != nil {
			t.Errorf("alias %q: %v", alias, err)
			continue
		}
		if found.Name() != object {
			t.Errorf("alias %q resolved to %q, want %q", alias, found.Name(), object)
		}
	}
}

func TestListDefaultFormatIsTable(t *testing.T) {
	if got := config.NewListConfig().Format; got != config.ListFormatTable {
		t.Errorf("NewListConfig default format = %q, want %q", got, config.ListFormatTable)
	}

	cmd := newListCmd()
	found, _, err := cmd.Find([]string{"databases"})
	if err != nil {
		t.Fatal(err)
	}
	flag := found.Flags().Lookup("format")
	if flag == nil {
		t.Fatal("--format flag not registered")
	}
	if flag.DefValue != config.ListFormatTable {
		t.Errorf("--format default = %q, want %q", flag.DefValue, config.ListFormatTable)
	}

	for _, removed := range []string{"tsv", "csv", "jsonl"} {
		if config.ValidListFormat(removed) {
			t.Errorf("%q must not be accepted", removed)
		}
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
	if !strings.Contains(err.Error(), "list requires an object") {
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
	if !strings.Contains(err.Error(), `unknown object "tabel"`) {
		t.Errorf("error = %v", err)
	}
}
