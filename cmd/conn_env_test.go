package cmd

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// The six connection options also read IMEX_HOST, IMEX_PORT, IMEX_USER,
// IMEX_PASSWORD, IMEX_SOCKET and IMEX_DATABASE, so a container or CI job can set
// them once instead of repeating them on every call. The rule these tests pin is
// "explicit flag > environment > built-in default", where "explicit" means
// FlagSet.Changed(): what decides is whether the user typed the option, never
// whether its value happens to equal the default. Comparing values would make
// `imex export -u root` lose to IMEX_USER=svc while `imex export -u admin` won,
// which is not a rule anyone could predict.
//
// connEnvFlags parses args the way the real command does and hands back the flag
// set plus a config holding the flag defaults. The flags bind to the package
// exportCfg, so it is reset to defaults first — the same starting point RunE has.
func connEnvFlags(t *testing.T, args ...string) (*pflag.FlagSet, *config.Config) {
	t.Helper()
	*exportCfg = *newExportConfigForTest()
	cmd := &cobra.Command{Use: "export"}
	registerExportFlags(cmd)
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("parse %q: %v", args, err)
	}
	cfg := exportCfg.Config
	return cmd.Flags(), &cfg
}

func TestConnEnvFillsUnsetOptions(t *testing.T) {
	t.Setenv("IMEX_HOST", "db.internal")
	t.Setenv("IMEX_PORT", "3307")
	t.Setenv("IMEX_USER", "svc")
	t.Setenv("IMEX_PASSWORD", "s3cr3t")
	t.Setenv("IMEX_SOCKET", "")
	t.Setenv("IMEX_DATABASE", "mydb")

	f, cfg := connEnvFlags(t)
	rep, err := applyConnEnv(f, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Host != "db.internal" || cfg.Port != 3307 || cfg.User != "svc" ||
		cfg.Password != "s3cr3t" || cfg.Database != "mydb" || cfg.Socket != "" {
		t.Errorf("environment did not fill the options: %+v", *cfg)
	}
	want := []string{"IMEX_HOST", "IMEX_PORT", "IMEX_USER", "IMEX_PASSWORD", "IMEX_DATABASE"}
	if strings.Join(rep.used, ",") != strings.Join(want, ",") {
		t.Errorf("used = %v, want %v", rep.used, want)
	}
	if len(rep.skipped) != 0 {
		t.Errorf("skipped = %v, want none", rep.skipped)
	}
}

// The order of connEnvSources fixes the order of the names in the INFO record,
// and the record must never carry a value: passwords arrive here as ENC:<...>
// strings that must not end up in the log or the --messages file.
func TestConnEnvSourceOrderAndReportIsNamesOnly(t *testing.T) {
	if got := connEnvSources[0].env; got != "IMEX_HOST" {
		t.Errorf("first source = %q, want IMEX_HOST", got)
	}
	if got := connEnvSources[len(connEnvSources)-1].env; got != "IMEX_DATABASE" {
		t.Errorf("last source = %q, want IMEX_DATABASE", got)
	}

	f, cfg := connEnvFlags(t)
	t.Setenv("IMEX_PASSWORD", "ENC:/tmp/key:abcdef")
	rep, err := applyConnEnv(f, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(rep.used, ",") != "IMEX_PASSWORD" {
		t.Errorf("used = %v, want only IMEX_PASSWORD", rep.used)
	}
	if strings.Contains(strings.Join(rep.used, ","), "abcdef") {
		t.Errorf("report leaked a value: %v", rep.used)
	}
	if cfg.Password != "ENC:/tmp/key:abcdef" {
		t.Errorf("password = %q", cfg.Password)
	}
}

func TestConnEnvExplicitFlagBeatsEnvironment(t *testing.T) {
	t.Setenv("IMEX_HOST", "db.internal")
	t.Setenv("IMEX_PORT", "3307")
	t.Setenv("IMEX_USER", "svc")
	t.Setenv("IMEX_PASSWORD", "fromenv")
	t.Setenv("IMEX_DATABASE", "fromenv_db")

	f, cfg := connEnvFlags(t,
		"-h", "127.0.0.1", "-P", "3306", "-u", "root", "-p", "", "-D", "mydb")
	rep, err := applyConnEnv(f, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Every value equals the built-in default except the empty password, which
	// is the default too: a typed option must still win over the environment.
	if cfg.Host != "127.0.0.1" || cfg.Port != 3306 || cfg.User != "root" ||
		cfg.Password != "" || cfg.Database != "mydb" {
		t.Errorf("environment overwrote explicit flags: %+v", *cfg)
	}
	if len(rep.used) != 0 || len(rep.skipped) != 0 {
		t.Errorf("report = used %v, skipped %v; want both empty", rep.used, rep.skipped)
	}
}

// A blank variable is unset, so `export IMEX_HOST=` keeps the built-in default
// instead of asking for a connection to the empty host. Leading and trailing
// space from a shell quoting mistake is trimmed rather than sent to the server.
func TestConnEnvBlankIsUnsetAndSpacesTrimmed(t *testing.T) {
	t.Setenv("IMEX_HOST", "")
	t.Setenv("IMEX_USER", "  svc  ")

	f, cfg := connEnvFlags(t)
	rep, err := applyConnEnv(f, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Host != "127.0.0.1" {
		t.Errorf("blank IMEX_HOST = %q, want the flag default", cfg.Host)
	}
	if cfg.User != "svc" {
		t.Errorf("IMEX_USER = %q, want it trimmed", cfg.User)
	}
	if strings.Join(rep.used, ",") != "IMEX_USER" {
		t.Errorf("used = %v, want IMEX_USER only", rep.used)
	}
}

// The port is checked where it is read, so the message names the variable the
// user actually set. It is a usage error (exit 2), like every other bad option.
func TestConnEnvPortMustBeValid(t *testing.T) {
	for _, tc := range []struct {
		value  string
		reject bool
	}{
		{"3307", false},
		{"1", false},
		{"65535", false},
		{"0", true},
		{"65536", true},
		{"abc", true},
		{"", false}, // blank means unset, not an invalid number
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("IMEX_PORT", tc.value)
			f, cfg := connEnvFlags(t)
			_, err := applyConnEnv(f, cfg)
			if tc.reject != (err != nil) {
				t.Fatalf("IMEX_PORT=%q err = %v, want rejected: %v", tc.value, err, tc.reject)
			}
			if err == nil {
				return
			}
			if !common.IsUsageError(err) {
				t.Errorf("IMEX_PORT=%q is a runtime error, not a usage error: %v", tc.value, err)
			}
			if !strings.Contains(err.Error(), "invalid IMEX_PORT") {
				t.Errorf("error names the wrong source: %v", err)
			}
			if cfg.Port != 3306 {
				t.Errorf("rejected value was still applied: Port = %d", cfg.Port)
			}
		})
	}
}

// The host/port pair and the socket are alternative transports, so an explicit
// address wins outright: IMEX_SOCKET is not applied and the user is told which
// variable was skipped. A socket named on the command line is still a socket, so
// IMEX_HOST is simply not consulted and nothing is reported as skipped.
func TestConnEnvTransportChoice(t *testing.T) {
	t.Setenv("IMEX_SOCKET", "/tmp/my.sock")

	f, cfg := connEnvFlags(t, "-h", "10.0.0.1")
	rep, err := applyConnEnv(f, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Socket != "" {
		t.Errorf("explicit --host with IMEX_SOCKET set: Socket = %q, want empty", cfg.Socket)
	}
	if strings.Join(rep.skipped, ",") != "IMEX_SOCKET" {
		t.Errorf("skipped = %v, want IMEX_SOCKET", rep.skipped)
	}
	if strings.Join(rep.used, ",") != "" {
		t.Errorf("used = %v, want none", rep.used)
	}
	if dsn := common.BuildDSN(cfg); !strings.Contains(dsn, "@tcp(10.0.0.1:3306)") {
		t.Errorf("DSN should stay on TCP: %s", dsn)
	}

	f, cfg = connEnvFlags(t, "-S", "/tmp/explicit.sock")
	rep, err = applyConnEnv(f, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Host != "127.0.0.1" {
		t.Errorf("explicit --socket must leave the host default alone: %q", cfg.Host)
	}
	if len(rep.used) != 0 || len(rep.skipped) != 0 {
		t.Errorf("report = used %v, skipped %v; want both empty", rep.used, rep.skipped)
	}
	if cfg.Socket != "/tmp/explicit.sock" {
		t.Errorf("Socket = %q", cfg.Socket)
	}

	f, cfg = connEnvFlags(t)
	rep, err = applyConnEnv(f, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Socket != "/tmp/my.sock" {
		t.Errorf("IMEX_SOCKET without an address must be applied: Socket = %q", cfg.Socket)
	}
	if strings.Join(rep.used, ",") != "IMEX_SOCKET" {
		t.Errorf("used = %v, want IMEX_SOCKET", rep.used)
	}
	if dsn := common.BuildDSN(cfg); !strings.Contains(dsn, "@unix(/tmp/my.sock)") {
		t.Errorf("DSN is not a unix connection: %s", dsn)
	}

	// Nothing named on either side: the socket from the environment still wins
	// over the default host, because a bare "127.0.0.1" is not a user request.
	t.Setenv("IMEX_HOST", "db.internal")
	f, cfg = connEnvFlags(t)
	if _, err := applyConnEnv(f, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Socket != "/tmp/my.sock" {
		t.Errorf("IMEX_SOCKET must beat IMEX_HOST when neither is a flag: Socket = %q", cfg.Socket)
	}
	if cfg.Host != "db.internal" {
		t.Errorf("Host = %q, want the environment value as well", cfg.Host)
	}
}

// -P alone also names an address: a socket would ignore the port anyway, so the
// port makes the run TCP.
func TestConnEnvExplicitPortIsAlsoAnAddress(t *testing.T) {
	t.Setenv("IMEX_SOCKET", "/tmp/my.sock")
	f, cfg := connEnvFlags(t, "-P", "3307")
	rep, err := applyConnEnv(f, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Socket != "" {
		t.Errorf("Socket = %q, want the socket skipped", cfg.Socket)
	}
	if cfg.Port != 3307 {
		t.Errorf("Port = %d, want 3307", cfg.Port)
	}
	if strings.Join(rep.skipped, ",") != "IMEX_SOCKET" {
		t.Errorf("skipped = %v, want IMEX_SOCKET", rep.skipped)
	}
}

// The two records are the only place a run says where its values came from, and
// they go to the same sink as everything else, i.e. into the --messages file too.
// So they must name variables and never carry a value.
func TestConnEnvLogRecordsNameVariablesOnly(t *testing.T) {
	orig := common.Logger
	t.Cleanup(func() { common.Logger = orig })

	var buf bytes.Buffer
	common.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	logConnEnv(connEnvReport{
		used:    []string{"IMEX_USER", "IMEX_PASSWORD"},
		skipped: []string{"IMEX_SOCKET"},
	})

	out := buf.String()
	if !strings.Contains(out, "msg=\"connection values supplied by environment\"") ||
		!strings.Contains(out, `variables="[IMEX_USER IMEX_PASSWORD]"`) {
		t.Errorf("INFO record missing or misordered:\n%s", out)
	}
	if !strings.Contains(out, "msg=\"connection variable ignored\"") ||
		!strings.Contains(out, "variable=IMEX_SOCKET") {
		t.Errorf("WARN record missing:\n%s", out)
	}
	// One record = one physical line, so a grep for either phrase returns a
	// complete record rather than half of one.
	if strings.Count(out, "\n") != 2 {
		t.Errorf("want 2 records on 2 lines, got:\n%s", out)
	}
	if strings.Contains(out, "/tmp/my.sock") || strings.Contains(out, "s3cr3t") {
		t.Errorf("a value reached the log: %s", out)
	}
}

// list and import go through the same helper, so a run configured entirely by
// the environment is complete before the required-option check runs.
func TestConnEnvAppliesToImportAndList(t *testing.T) {
	t.Setenv("IMEX_HOST", "db.internal")
	t.Setenv("IMEX_PORT", "3307")
	t.Setenv("IMEX_USER", "svc")
	t.Setenv("IMEX_PASSWORD", "pw")
	t.Setenv("IMEX_DATABASE", "mydb")
	t.Setenv("IMEX_SOCKET", "")

	imp := &cobra.Command{Use: "import"}
	*importCfg = *newImportConfigForTest()
	registerImportFlags(imp)
	if err := imp.Flags().Parse([]string{"-i", "/tmp/in.sql", "-t", "users"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	rep, err := applyConnEnv(imp.Flags(), &importCfg.Config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rep.used) != 5 {
		t.Errorf("used = %v, want the five connection variables", rep.used)
	}
	// The required-option check now sees a complete configuration, so it must not
	// ask for a host the environment supplied. Any other complaint is fine here:
	// the file does not exist, but that is not what this case is about.
	if err := validateImportConfig(importCmd, importCfg); err != nil &&
		strings.Contains(err.Error(), "--host") {
		t.Errorf("import still asks for a host the environment supplied: %v", err)
	}

	listCmd := &cobra.Command{Use: "tables"}
	cfg := config.NewListConfig()
	registerListFlags(listCmd, cfg)
	if err := listCmd.Flags().Parse([]string{}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := applyConnEnv(listCmd.Flags(), &cfg.Config); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Host != "db.internal" || cfg.Port != 3307 || cfg.User != "svc" ||
		cfg.Password != "pw" || cfg.Database != "mydb" {
		t.Errorf("list did not read the environment: %+v", cfg.Config)
	}
}

// A required-parameter message has to name what the operator can act on. The
// password check used to read `password is required`, naming neither the option
// nor the variable — and a password is the field most likely to come from the
// environment rather than the command line, so that reader has the least way to
// guess which of the two to set. All three commands that connect now emit the same
// string, naming --password and IMEX_PASSWORD.
func TestMissingPasswordMessageNamesBothSources(t *testing.T) {
	const want = "--password is required (or set IMEX_PASSWORD)"

	// Blank means unset, so the environment cannot supply the value here either.
	t.Setenv("IMEX_PASSWORD", "")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"export", []string{"export", "-h", "127.0.0.1", "-u", "root", "-D", "mydb",
			"-o", "/tmp/imex-missing-password.del", "-q", "SELECT 1"}},
		{"import", []string{"import", "-h", "127.0.0.1", "-u", "root", "-D", "mydb",
			"-i", "/tmp/imex-missing-password.del", "-t", "users"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*exportCfg = *newExportConfigForTest()
			*importCfg = *newImportConfigForTest()
			logFormat = "console"
			resetChangedFlags(exportCmd, importCmd, rootCmd)
			rootCmd.SetArgs(tc.args)
			err := rootCmd.Execute()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: want %q, got %v", tc.name, want, err)
			}
			if !common.IsUsageError(err) {
				t.Errorf("%s: want a usage error (exit code 2), got %T", tc.name, err)
			}
		})
	}

	cfg := config.NewListConfig()
	cfg.Host, cfg.User, cfg.Database, cfg.Table, cfg.Password = "127.0.0.1", "root", "mydb", "users", ""
	err := validateListConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("list: want %q, got %v", want, err)
	}
}
