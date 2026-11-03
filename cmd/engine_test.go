package cmd

// Tests for the --engine face of the CLI. They pin three things at once:
//
//   1. the exit-2 refusal matrix for --engine pg, with the message text a user
//      actually reads (contract face ③), measured from the built binary before
//      these expectations were written down;
//   2. that --engine mysql accepts every option the pg path refuses — the
//      refactor must not cost the existing engine a single usable flag;
//   3. that the engine arrives only from the flag: IMEX_ENGINE does not exist, so
//      face ④ (the IMEX_* environment) is unchanged by this feature.
//
// The refusals are driven through validateOnly / the config validators rather
// than rootCmd.Execute() so no case dials a server: every one of them fires
// before the first connection, which is itself part of the contract.

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
	"github.com/imex/internal/list"
	"github.com/imex/internal/show"
)

// pgConnFlags is the shortest connection set that passes every non-engine check,
// so a case below fails only on the engine rule it is about. The address is
// deliberately unreachable: reaching the server would mean the check missed.
var pgConnFlags = []string{"-h", "127.0.0.1", "-P", "5432", "-u", "pgprobe", "-p", "pw", "-D", "pgprobe"}

func pgExportFlags(extra ...string) []string {
	return append(append([]string{}, pgConnFlags...), append([]string{"--engine", "pg", "-o", "/tmp/imex-engine-test.del", "-q", "SELECT 1"}, extra...)...)
}

func pgImportFlags(extra ...string) []string {
	return append(append([]string{}, pgConnFlags...), append([]string{"--engine", "pg", "-i", "/tmp/imex-engine-test.del", "-t", "rt"}, extra...)...)
}

// TestEngineRefusalMatrix drives every option --engine pg cannot serve through the
// real validator and asserts the message and the exit class. One table, both
// directions, so a new engine refusal without a row here cannot land quietly.
func TestEngineRefusalMatrix(t *testing.T) {
	cases := []struct {
		name string
		sub  string
		args []string
		want string
	}{
		{
			name: "export_unknown_engine",
			sub:  "export",
			args: append(append([]string{}, pgConnFlags...), "-o", "/tmp/x.del", "-q", "SELECT 1", "--engine", "oracle"),
			want: `invalid --engine "oracle": must be one of mysql/pg`,
		},
		{
			name: "export_format_sql",
			sub:  "export",
			args: pgExportFlags("-f", "sql"),
			want: "--format sql is not supported with --engine pg: the SQL writer emits MySQL INSERT text (backquoted names and MySQL escaping) that no PostgreSQL server accepts, use --format del or --format parquet",
		},
		{
			name: "export_threads",
			sub:  "export",
			args: pgExportFlags("--threads", "4"),
			want: "--threads is not supported with --engine pg: parallel export shards the query with MySQL-only statements and has not been measured against PostgreSQL, use --threads 1",
		},
		{
			name: "export_no_hex_blob",
			sub:  "export",
			args: pgExportFlags("--no-hex-blob"),
			want: "--no-hex-blob is not supported with --engine pg: a PostgreSQL server rejects a NUL inside any text value (SQLSTATE 22021), so bytea columns must go out as 0x hex; imex always does it on this engine",
		},
		{
			name: "export_session_var",
			sub:  "export",
			args: pgExportFlags("--session-var", "sql_mode=STRICT_ALL_TABLES"),
			want: "--session-var is not supported with --engine pg: SET <name>=<value> is MySQL session grammar and imex does not translate it, put PostgreSQL settings in the server or role configuration",
		},
		{
			name: "export_charset",
			sub:  "export",
			args: pgExportFlags("--default-charset", "nonsense"),
			want: `invalid --default-charset "nonsense" with --engine pg: PostgreSQL has no such client encoding (supported: utf8mb4, latin1, gbk, gb18030, ascii)`,
		},
		{
			name: "import_format_sql",
			sub:  "import",
			args: pgImportFlags("-f", "sql"),
			want: "--format sql is not supported with --engine pg: a SQL dump is parsed with the MySQL statement grammar and carries MySQL INSERT text, import a --format del or --format parquet file instead",
		},
		{
			name: "import_mode_replace",
			sub:  "import",
			args: pgImportFlags("--mode", "replace"),
			want: "--mode replace is not supported with --engine pg: it needs DDL or an upsert that PostgreSQL spells differently and imex will not guess, use --mode insert",
		},
		{
			name: "import_mode_insert_update",
			sub:  "import",
			args: pgImportFlags("--mode", "insert_update"),
			want: "--mode insert_update is not supported with --engine pg: it needs DDL or an upsert that PostgreSQL spells differently and imex will not guess, use --mode insert",
		},
		{
			name: "import_mode_create",
			sub:  "import",
			args: pgImportFlags("--mode", "create"),
			want: "--mode create is not supported with --engine pg: it needs DDL or an upsert that PostgreSQL spells differently and imex will not guess, use --mode insert",
		},
		{
			name: "import_threads",
			sub:  "import",
			args: pgImportFlags("--threads", "4"),
			want: "--threads is not supported with --engine pg: parallel import has not been measured against PostgreSQL, use --threads 1",
		},
		{
			name: "import_session_var",
			sub:  "import",
			args: pgImportFlags("--session-var", "sql_mode=STRICT_ALL_TABLES"),
			want: "--session-var is not supported with --engine pg: SET <name>=<value> is MySQL session grammar and imex does not translate it, put PostgreSQL settings in the server or role configuration",
		},
		{
			name: "import_charset",
			sub:  "import",
			args: pgImportFlags("--default-charset", "nonsense"),
			want: `invalid --default-charset "nonsense" with --engine pg: PostgreSQL has no such client encoding (supported: utf8mb4, latin1, gbk, gb18030, ascii)`,
		},
		{
			name: "import_engine_typo",
			sub:  "import",
			args: append(append([]string{}, pgConnFlags...), "-i", "/tmp/x.del", "-t", "rt", "--engine", "postgres"),
			want: `invalid --engine "postgres": must be one of mysql/pg`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateOnly(t, c.sub, c.args)
			if err == nil {
				t.Fatalf("expected refusal for %v", c.args)
			}
			if err.Error() != c.want {
				t.Errorf("message drifted:\n got %q\nwant %q", err.Error(), c.want)
			}
			if !common.IsUsageError(err) {
				t.Errorf("error %q must be a usage error (exit code 2)", err)
			}
		})
	}
}

// TestListAndShowRefusals drives the two commands that hold their own connection.
// Their engine checks live in the validators, so a caller that builds a config by
// hand hits the same wall as the command line does.
func TestListAndShowRefusals(t *testing.T) {
	newListCfg := func(object string) *config.ListConfig {
		cfg := config.NewListConfig()
		cfg.Engine = "pg"
		cfg.Host = "127.0.0.1"
		cfg.Port = 5432
		cfg.User = "pgprobe"
		cfg.Password = "pw"
		cfg.Database = "pgprobe"
		cfg.Object = object
		return cfg
	}

	events := newListCfg(list.ObjectEvents)
	err := validateListConfig(events)
	if err == nil || err.Error() != "list events is not supported with --engine pg: PostgreSQL has no event scheduler, so there is nothing to list; scheduled work lives in cron or pgAgent" {
		t.Fatalf("list events refusal = %v", err)
	}
	if !common.IsUsageError(err) {
		t.Errorf("list events refusal must exit 2, got %q", err)
	}

	// The other seven objects are supported, and validation must say nothing about
	// the engine for them.
	for _, object := range list.Objects() {
		if object == list.ObjectEvents {
			continue
		}
		cfg := newListCfg(object)
		if object == list.ObjectColumns || object == list.ObjectIndexes {
			cfg.Table = "rt"
		}
		if object == list.ObjectTables || object == list.ObjectRoutines {
			cfg.Type = "all"
		}
		if err := validateListConfig(cfg); err != nil {
			t.Errorf("list %s on pg must validate: %v", object, err)
		}
	}

	for _, kind := range show.Kinds() {
		err := checkShowEngineOptions(backend.EnginePG, kind.Short)
		want := "show " + kind.Short + " is not supported with --engine pg: definitions are read with SHOW CREATE, which PostgreSQL does not have, and imex cannot import the DDL it would produce; use pg_dump"
		if err == nil || err.Error() != want {
			t.Errorf("show %s refusal = %v, want %q", kind.Short, err, want)
		}
		if !common.IsUsageError(err) {
			t.Errorf("show %s refusal must exit 2", kind.Short)
		}
	}

	// A charset PostgreSQL cannot name is refused by `show` too, through the
	// validator rather than the kind check.
	cfg := config.NewShowConfig()
	cfg.Engine = "pg"
	cfg.Host = "127.0.0.1"
	cfg.Port = 5432
	cfg.User = "pgprobe"
	cfg.Password = "pw"
	cfg.Database = "pgprobe"
	cfg.DefaultCharset = "nonsense"
	err = validateShowConfig(cfg)
	if err == nil || !strings.HasPrefix(err.Error(), `invalid --default-charset "nonsense" with --engine pg`) {
		t.Fatalf("show charset refusal = %v", err)
	}
	if !common.IsUsageError(err) {
		t.Errorf("show charset refusal must exit 2, got %q", err)
	}
}

// TestMySQLOptionsAreUntouched is the other half of the increment: every option
// the pg path refuses must still validate cleanly on the default engine, or the
// refactor has quietly narrowed what a MySQL run can do.
func TestMySQLOptionsAreUntouched(t *testing.T) {
	base := []string{"-h", "127.0.0.1", "-P", "3306", "-u", "root", "-p", "pw", "-D", "mydb"}
	cases := []struct {
		name string
		sub  string
		args []string
	}{
		{"export_sql_threads_sessionvar", "export", append(append([]string{}, base...), "-o", "/tmp/x.sql", "-q", "SELECT 1", "-f", "sql", "--threads", "4", "--session-var", "sql_mode=ALL", "--no-hex-blob")},
		{"export_default_engine", "export", append(append([]string{}, base...), "-o", "/tmp/x.sql", "-q", "SELECT 1", "-f", "sql", "--threads", "4")},
		{"import_modes", "import", append(append([]string{}, base...), "-i", "/tmp/x.sql", "-t", "rt", "-f", "sql", "--mode", "create")},
		{"import_replace_threads", "import", append(append([]string{}, base...), "-i", "/tmp/x.del", "-t", "rt", "--mode", "replace", "--threads", "8", "--session-var", "sql_mode=ALL")},
		{"import_insert_update", "import", append(append([]string{}, base...), "-i", "/tmp/x.del", "-t", "rt", "--mode", "insert_update")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateOnly(t, c.sub, c.args); err != nil {
				t.Errorf("mysql run refused %v: %v", c.args, err)
			}
		})
	}

	// list and show keep working on mysql, events included.
	cfg := config.NewListConfig()
	cfg.Engine = "mysql"
	cfg.Host = "127.0.0.1"
	cfg.Port = 3306
	cfg.User = "root"
	cfg.Password = "pw"
	cfg.Object = list.ObjectEvents
	if err := validateListConfig(cfg); err != nil {
		t.Errorf("list events on mysql must validate: %v", err)
	}

	// The engine name is matched case-insensitively and blank-tolerant, so a
	// hand-written config that says "MySQL" still means mysql — and still gets
	// events, which only the pg path refuses.
	norm := config.NewListConfig()
	norm.Engine = "MySQL"
	norm.Host = "127.0.0.1"
	norm.Port = 3306
	norm.User = "root"
	norm.Password = "pw"
	norm.Object = list.ObjectEvents
	if err := validateListConfig(norm); err != nil {
		t.Errorf(`--engine "MySQL" must normalise to mysql, got %v`, err)
	}

	pg := config.NewListConfig()
	pg.Engine = " PG "
	pg.Host = "127.0.0.1"
	pg.Port = 5432
	pg.User = "pgprobe"
	pg.Password = "pw"
	pg.Object = list.ObjectEvents
	if err := validateListConfig(pg); err == nil || !strings.Contains(err.Error(), "list events is not supported with --engine pg") {
		t.Errorf(`--engine " PG " must normalise to pg and refuse events, got %v`, err)
	}
}

// TestEnginePortIsAppliedByEngineOnly pins what --engine pg does to the port and,
// just as much, what it does not do: an explicit --port or an IMEX_PORT always
// wins, and a mysql run never sees 5432.
func TestEnginePortIsAppliedByEngineOnly(t *testing.T) {
	cases := []struct {
		name    string
		engine  string
		portArg string
		envPort string
		want    int
	}{
		{"pg_default", "pg", "", "", 5432},
		{"pg_explicit_port", "pg", "6543", "", 6543},
		{"pg_env_port", "pg", "", "7000", 7000},
		{"mysql_default", "mysql", "", "", 3306},
		{"mysql_pg_port_kept", "mysql", "5432", "", 5432},
		{"empty_engine_default", "", "", "", 3306},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.envPort != "" {
				t.Setenv("IMEX_PORT", c.envPort)
			}
			got := resolvedExportPort(t, c.engine, c.portArg)
			if got != c.want {
				t.Errorf("engine %q port-flag %q env %q: port = %d, want %d", c.engine, c.portArg, c.envPort, got, c.want)
			}
		})
	}
}

// resolvedExportPort runs the same three steps runExport does before it dials:
// fill from the environment, resolve the engine, apply what the engine decides.
func resolvedExportPort(t *testing.T, engine, portArg string) int {
	t.Helper()
	*exportCfg = *newExportConfigForTest()
	resetChangedFlags(exportCmd, importCmd, rootCmd)

	args := []string{"-h", "127.0.0.1", "-u", "root", "-p", "pw", "-D", "mydb", "-o", "/tmp/x.del", "-q", "SELECT 1"}
	if engine != "" {
		args = append(args, "--engine", engine)
	}
	if portArg != "" {
		args = append(args, "-P", portArg)
	}
	if err := exportCmd.Flags().Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	cfg := exportCfg
	if _, err := applyConnEnv(exportCmd.Flags(), &cfg.Config); err != nil {
		t.Fatalf("apply conn env: %v", err)
	}
	e, err := parseEngine(cfg.Engine)
	if err != nil {
		t.Fatalf("parse engine: %v", err)
	}
	applyEnginePort(exportCmd, &cfg.Config, e)
	if err := validatePort(cfg.Port); err != nil {
		t.Fatalf("port %d: %v", cfg.Port, err)
	}
	return cfg.Port
}

// TestEngineHasNoEnvironmentVariable keeps contract face ④ as it was measured
// before this feature: the engine comes from --engine and from nowhere else, so
// an IMEX_* sweep finds no engine knob.
func TestEngineHasNoEnvironmentVariable(t *testing.T) {
	t.Setenv("IMEX_ENGINE", "pg")

	*exportCfg = *newExportConfigForTest()
	resetChangedFlags(exportCmd, importCmd, rootCmd)
	args := []string{"-h", "127.0.0.1", "-P", "3306", "-u", "root", "-p", "pw", "-D", "mydb",
		"-o", "/tmp/x.del", "-q", "SELECT 1"}
	if err := exportCmd.Flags().Parse(args); err != nil {
		t.Fatal(err)
	}
	cfg := exportCfg
	if _, err := applyConnEnv(exportCmd.Flags(), &cfg.Config); err != nil {
		t.Fatal(err)
	}
	if cfg.Engine != "mysql" {
		t.Fatalf("IMEX_ENGINE changed the engine to %q: the flag is the only source", cfg.Engine)
	}

	for _, src := range connEnvSources {
		if src.env == "IMEX_ENGINE" || src.flag == "engine" {
			t.Fatalf("connection env table grew an engine entry: %s/%s", src.flag, src.env)
		}
	}
}

// TestEngineFlagFace pins the --engine flag on the sixteen help faces that carry
// it: export, import, the eight `list` objects and the six `show` kinds. The
// default must stay mysql, so an existing run's behaviour never depends on this
// feature being there, and the usage text is what docs/cli-oracle.txt quotes.
func TestEngineFlagFace(t *testing.T) {
	faces := map[string]*cobra.Command{
		"export": exportCmd,
		"import": importCmd,
	}
	for _, sub := range newListCmd().Commands() {
		faces["list "+sub.Use] = sub
	}
	for _, sub := range newShowCmd().Commands() {
		faces["show "+sub.Use] = sub
	}
	if len(faces) != 16 {
		t.Fatalf("engine flag faces = %d, want 16: %v", len(faces), faces)
	}
	for name, cmd := range faces {
		f := cmd.Flags().Lookup("engine")
		if f == nil {
			t.Fatalf("%s has no --engine flag", name)
		}
		if f.DefValue != "mysql" {
			t.Errorf("%s --engine default = %q, want mysql", name, f.DefValue)
		}
		if f.Usage != "Database engine: mysql, pg" {
			t.Errorf("%s --engine usage = %q", name, f.Usage)
		}
	}
}
