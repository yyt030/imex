package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

func validExecCfg() *config.ExecConfig {
	cfg := config.NewExecConfig()
	cfg.Password = "pass"
	return cfg
}

// The exec command talks to a server, so it keeps the same channel flags list and
// show keep, and it must not steal -h from --host.
func TestExecCommandRegistersFlags(t *testing.T) {
	cmd := newExecCmd()
	for flagName, shorthand := range map[string]string{
		"output":       "o",
		"file":         "f",
		"messages":     "m",
		"database":     "D",
		"help":         "?",
		"no-overwrite": "",
		"no-col-names": "",
	} {
		flag := cmd.Flags().Lookup(flagName)
		if flag == nil {
			t.Errorf("--%s missing", flagName)
			continue
		}
		if flag.Shorthand != shorthand {
			t.Errorf("--%s shorthand = %q, want %q", flagName, flag.Shorthand, shorthand)
		}
	}
	if flag := cmd.Flags().Lookup("host"); flag == nil || flag.Shorthand != "h" {
		t.Errorf("-h must stay --host, got %v", flag)
	}
	// exec has no --pattern (enumeration is list's) and no --query (it is the query).
	for _, absent := range []string{"pattern", "query"} {
		if cmd.Flags().Lookup(absent) != nil {
			t.Errorf("--%s must not exist on exec", absent)
		}
	}
}

func TestResolveExecSQLSources(t *testing.T) {
	t.Run("file and argument are mutually exclusive", func(t *testing.T) {
		cfg := validExecCfg()
		cfg.File = "/tmp/a.sql"
		_, err := resolveExecSQL(cfg, []string{"SELECT 1"})
		if err == nil || !common.IsUsageError(err) || !strings.Contains(err.Error(), "either --file or a SQL argument") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("more than one argument", func(t *testing.T) {
		cfg := validExecCfg()
		_, err := resolveExecSQL(cfg, []string{"SELECT", "1"})
		if err == nil || !common.IsUsageError(err) || !strings.Contains(err.Error(), "at most one SQL argument") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("single argument is the SQL", func(t *testing.T) {
		cfg := validExecCfg()
		got, err := resolveExecSQL(cfg, []string{"SELECT 1; SELECT 2;"})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if got != "SELECT 1; SELECT 2;" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("file is read from disk", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "s.sql")
		if err := os.WriteFile(path, []byte("CREATE TABLE t (a INT);"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := validExecCfg()
		cfg.File = path
		got, err := resolveExecSQL(cfg, nil)
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if got != "CREATE TABLE t (a INT);" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("missing file is not a usage error", func(t *testing.T) {
		cfg := validExecCfg()
		cfg.File = filepath.Join(t.TempDir(), "nope.sql")
		_, err := resolveExecSQL(cfg, nil)
		if err == nil {
			t.Fatal("want an error for a missing file")
		}
		if common.IsUsageError(err) {
			t.Errorf("a missing file is a runtime error, not a usage error")
		}
	})
	t.Run("stdin is read when no other source", func(t *testing.T) {
		prev := execStdin
		execStdin = strings.NewReader("SELECT 1;")
		defer func() { execStdin = prev }()
		cfg := validExecCfg()
		got, err := resolveExecSQL(cfg, nil)
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if got != "SELECT 1;" {
			t.Errorf("got %q", got)
		}
	})
}

func TestValidateExecConfigRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		edit func(*config.ExecConfig)
		want string
	}{
		{"password", func(c *config.ExecConfig) { c.Password = "" }, "--password is required (or set IMEX_PASSWORD)"},
		{"user", func(c *config.ExecConfig) { c.User = "" }, "--user is required"},
		{"host", func(c *config.ExecConfig) { c.Host = "" }, "--host is required"},
		{"port", func(c *config.ExecConfig) { c.Port = 0 }, "invalid --port"},
		{"log level", func(c *config.ExecConfig) { c.LogLevel = "verbose" }, "invalid --log-level"},
		{"charset", func(c *config.ExecConfig) { c.DefaultCharset = "klingon" }, "invalid --default-charset"},
		{"connect timeout", func(c *config.ExecConfig) { c.ConnectTimeout = 0 }, "invalid --connect-timeout"},
		{"read timeout", func(c *config.ExecConfig) { c.SocketReadTimeout = -1 }, "invalid --socket-read-timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validExecCfg()
			tc.edit(cfg)
			err := validateExecConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want substring %q", err, tc.want)
			}
			if !common.IsUsageError(err) {
				t.Fatalf("error must be a usage error, got %T", err)
			}
		})
	}
}

func TestValidateExecConfigRefusesResultAndMessageSameFile(t *testing.T) {
	prev := messagesPath
	messagesPath = "/tmp/exec-same.log"
	defer func() { messagesPath = prev }()

	cfg := validExecCfg()
	cfg.Output = "/tmp/exec-same.log"
	err := validateExecConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "--output and --messages must name different files") {
		t.Fatalf("got %v", err)
	}
	if !common.IsUsageError(err) {
		t.Errorf("error must be a usage error, got %T", err)
	}
}

// Every help line exec prints must stay within the byte budget the oracle keeps,
// covering the command's own Long text and its flag descriptions.
func TestExecHelpLinesStayWithinTheLimit(t *testing.T) {
	cmd := newExecCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if len(line) > 118 {
			t.Errorf("line is %d bytes: %q", len(line), line)
		}
	}
}
