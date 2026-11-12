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

// dmConnFlags is the same idea for --engine dm. The address stays unreachable: a
// refusal that fired has not dialled, and a case that reached the server would
// fail on a connection error instead of on the rule it is about.
var dmConnFlags = []string{"-h", "127.0.0.1", "-P", "5236", "-u", "dmprobe", "-p", "pw", "-D", "DMPROBE"}

func pgExportFlags(extra ...string) []string {
	return append(append([]string{}, pgConnFlags...), append([]string{"--engine", "pg", "-o", "/tmp/imex-engine-test.del", "-q", "SELECT 1"}, extra...)...)
}

func pgImportFlags(extra ...string) []string {
	return append(append([]string{}, pgConnFlags...), append([]string{"--engine", "pg", "-i", "/tmp/imex-engine-test.del", "-t", "rt"}, extra...)...)
}

func dmExportFlags(extra ...string) []string {
	return append(append([]string{}, dmConnFlags...), append([]string{"--engine", "dm", "-o", "/tmp/imex-engine-test.del", "-q", "SELECT 1"}, extra...)...)
}

func dmImportFlags(extra ...string) []string {
	return append(append([]string{}, dmConnFlags...), append([]string{"--engine", "dm", "-i", "/tmp/imex-engine-test.del", "-t", "RT"}, extra...)...)
}

// TestEngineRefusalMatrix drives every option an engine cannot serve through the
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
			want: `invalid --engine "oracle": must be one of mysql/pg/dm`,
		},
		{
			name: "export_no_hex_blob",
			sub:  "export",
			args: pgExportFlags("--no-hex-blob"),
			want: "--no-hex-blob is not supported with --engine pg: a PostgreSQL server rejects a NUL inside any text value (SQLSTATE 22021), so bytea columns must go out as 0x hex; imex always does it on this engine",
		},
		{
			name: "export_charset",
			sub:  "export",
			args: pgExportFlags("--default-charset", "nonsense"),
			want: `invalid --default-charset "nonsense" with --engine pg: PostgreSQL has no such client encoding (supported: utf8mb4, latin1, gbk, gb18030, ascii)`,
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
			want: `invalid --engine "postgres": must be one of mysql/pg/dm`,
		},

		// --engine dm. The MVP claims del/parquet/sql export (sql in the Dameng
		// grammar), del/parquet import in insert/replace mode, and four list
		// objects; everything else has to stop here rather than at the server.
		{
			name: "dm_export_engine_dm8",
			sub:  "export",
			args: append(append([]string{}, dmConnFlags...), "-o", "/tmp/x.del", "-q", "SELECT 1", "--engine", "dm8"),
			want: `invalid --engine "dm8": must be one of mysql/pg/dm`,
		},
		{
			name: "dm_export_threads",
			sub:  "export",
			args: dmExportFlags("--threads", "4"),
			want: "--threads is not supported with --engine dm: parallel export has not been measured against Dameng, use --threads 1",
		},
		{
			name: "dm_export_no_hex_blob",
			sub:  "export",
			args: dmExportFlags("--no-hex-blob"),
			want: "--no-hex-blob is not supported with --engine dm: Dameng rejects a NUL byte inside a text value, so binary columns must go out as 0x hex; imex always does it on this engine",
		},
		{
			name: "dm_export_session_var",
			sub:  "export",
			args: dmExportFlags("--session-var", "compatible_mode=oracle"),
			want: "--session-var is not supported with --engine dm: SET <name>=<value> is MySQL session grammar and imex does not translate it for Dameng; put session settings in the server or role configuration",
		},
		{
			name: "dm_import_format_sql",
			sub:  "import",
			args: dmImportFlags("-f", "sql"),
			want: "--format sql is not supported with --engine dm: imex can now export a Dameng-dialect --format sql dump, but the SQL reader still parses with the MySQL statement grammar and cannot replay a Dameng dump; import a --format del or --format parquet file, or load the dump with your own DM client",
		},
		{
			name: "dm_import_mode_insert_update",
			sub:  "import",
			args: dmImportFlags("--mode", "insert_update"),
			want: "--mode insert_update is not supported with --engine dm: the Dameng backend does not implement upsert in this MVP, use --mode insert or --mode replace",
		},
		{
			name: "dm_import_session_var",
			sub:  "import",
			args: dmImportFlags("--session-var", "compatible_mode=oracle"),
			want: "--session-var is not supported with --engine dm: SET <name>=<value> is MySQL session grammar and imex does not translate it for Dameng; put session settings in the server or role configuration",
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

// TestDMExportSQLIsNoLongerRefused pins the DM capability increment: --engine dm
// -f sql export used to stop at the CLI with a "no Dameng server accepts"
// refusal. Now that the SQL writer gained a Dameng/Oracle grammar it must pass
// validation (validateExportConfig returns nil, and no connection is attempted
// by validateOnly). The DM import path stays refused — see the matrix case
// dm_import_format_sql — so only export is asserted open here.
func TestDMExportSQLIsNoLongerRefused(t *testing.T) {
	if err := validateOnly(t, "export", dmExportFlags("-f", "sql")); err != nil {
		t.Fatalf("expected --engine dm -f sql export to validate, got: %v", err)
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

	// Only `event` is refused on pg: table/view/trigger/procedure/function are
	// rebuilt from the catalog (show_postgres.go), and event has no object because
	// PostgreSQL has no scheduler.
	for _, kind := range show.Kinds() {
		err := checkShowEngineOptions(backend.EnginePG, kind.Short)
		if kind.Short == "evt" {
			want := "show evt is not supported with --engine pg: PostgreSQL has no event scheduler, so there is no event definition to show; scheduled work lives in cron or pgAgent"
			if err == nil || err.Error() != want {
				t.Errorf("show evt refusal = %v, want %q", err, want)
			}
			if !common.IsUsageError(err) {
				t.Errorf("show evt refusal must exit 2")
			}
			continue
		}
		if err != nil {
			t.Errorf("show %s on pg must be supported, got refusal %v", kind.Short, err)
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

// TestPGExportOptionsAreAccepted is the export half of the same discipline: the
// --sql-dialect increment makes a SQL dump reachable on --engine pg, so the
// export_format_sql refusal has to land here rather than vanish without a
// counter-assertion, and --session-var is honoured the way MySQL honours it (a
// real name that is not tool- or driver-owned reaches the server as a SET).
// --threads above 1 joined this list when the parallel path learned the
// PostgreSQL catalogue; --no-hex-blob stays a refusal and is restated so
// accepting it later is a deliberate edit.
func TestPGExportOptionsAreAccepted(t *testing.T) {
	accepted := []struct {
		name string
		args []string
	}{
		{"export_sql_auto", pgExportFlags("-f", "sql", "-o", "/tmp/imex-engine-test.sql")},
		{"export_sql_pg", pgExportFlags("-f", "sql", "--sql-dialect", "pg", "-o", "/tmp/imex-engine-test.sql")},
		{"export_sql_mysql_grammar", pgExportFlags("-f", "sql", "--sql-dialect", "mysql", "-o", "/tmp/imex-engine-test.sql")},
		{"export_parquet", pgExportFlags("-f", "parquet", "-o", "/tmp/imex-engine-test.parquet")},
		{"export_session_var", pgExportFlags("--session-var", "work_mem=64MB", "--session-var", "application_name=imex")},
		{"export_threads", pgExportFlags("--threads", "4")},
		{"export_threads_with_sql", pgExportFlags("-f", "sql", "--threads", "4")},
	}
	for _, c := range accepted {
		t.Run(c.name, func(t *testing.T) {
			if err := validateOnly(t, "export", c.args); err != nil {
				t.Errorf("pg export refused %v: %v", c.args, err)
			}
		})
	}

	still := []struct {
		name string
		args []string
	}{
		{"no_hex_blob", pgExportFlags("--no-hex-blob")},
		{"bad_dialect", pgExportFlags("-f", "sql", "--sql-dialect", "oracle")},
		{"dialect_without_sql", pgExportFlags("-f", "del", "--sql-dialect", "pg")},
	}
	for _, c := range still {
		t.Run(c.name, func(t *testing.T) {
			if err := validateOnly(t, "export", c.args); err == nil {
				t.Errorf("%s must still be refused on --engine pg", c.name)
			}
		})
	}
}

// TestPGImportOptionsAreAccepted pins the other half of the import matrix: the
// options --engine pg now serves. A refusal case deleted from the table above has
// to reappear here, or nothing at all says which side the option belongs on.
// --threads, --mode replace and --mode insert_update were refusals until the port
// round measured them against a real 14.24 server.
func TestPGImportOptionsAreAccepted(t *testing.T) {
	type optCase struct {
		name string
		args []string
	}
	cases := []optCase{
		{"import_threads", pgImportFlags("--threads", "8")},
		{"import_replace", pgImportFlags("--mode", "replace")},
		{"import_replace_with_threads", pgImportFlags("--mode", "replace", "--threads", "4")},
		{"import_insert_update", pgImportFlags("--mode", "insert_update")},
		{"import_insert_update_with_threads", pgImportFlags("--mode", "insert_update", "--threads", "4")},
		{"import_parquet", pgImportFlags("-f", "parquet", "-i", "/tmp/imex-engine-test.parquet")},
		{"import_sql", pgImportFlags("-f", "sql", "-i", "/tmp/imex-engine-test.sql")},
		{"import_session_var", pgImportFlags("--session-var", "work_mem=64MB")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateOnly(t, "import", c.args); err != nil {
				t.Errorf("pg import refused %v: %v", c.args, err)
			}
		})
	}

	// Nothing is refused on the pg import path any more. --format sql moved to the
	// accepted list with the --sql-dialect increment (the reader parses the dump and
	// the importer re-binds it), --mode create is gone engine-blind (an invalid mode
	// value on every engine, asserted in TestCreateModeIsGone, not an engine-specific
	// refusal), and --session-var is now honoured the way MySQL honours it — a
	// tool- or driver-owned name like client_encoding fails in the parser, not as an
	// engine refusal, so it is pinned separately in TestPGSessionVarReservedNames.
}

// TestCreateModeIsGone locks the removal of --mode create: it is now an invalid
// mode value, engine-blind, rejected with the same message on the default engine
// and on pg. A mode that once mapped to a real code path must not linger as a
// silently-accepted no-op; giving it has to fail at validation, before any dial.
func TestCreateModeIsGone(t *testing.T) {
	want := `invalid --mode "create": must be one of insert/insert_update/replace`
	cases := []struct {
		name string
		sub  string
		args []string
	}{
		{"mysql", "import", append(append([]string{"-h", "127.0.0.1", "-P", "3306", "-u", "root", "-p", "pw", "-D", "mydb"}, "-i", "/tmp/x.sql", "-t", "rt", "-f", "sql"), "--mode", "create")},
		{"pg", "import", pgImportFlags("--mode", "create")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateOnly(t, c.sub, c.args)
			if err == nil || err.Error() != want {
				t.Fatalf("%s --mode create = %v, want %q", c.name, err, want)
			}
			if !common.IsUsageError(err) {
				t.Errorf("%s --mode create must exit 2, got %q", c.name, err)
			}
		})
	}
}

// TestPGSessionVarReservedNames pins the parser-side half of honouring
// --session-var on pg: a name the connection or the tool owns is rejected before
// the connection is built, exactly as on MySQL, while an ordinary GUC name is
// accepted and handed to the backend to SET. This is not the exit-2 engine
// refusal (that is gone) but the shared ParseSessionVars check, so it drives the
// parser directly rather than through a refusal table.
func TestPGSessionVarReservedNames(t *testing.T) {
	reserved := []string{"client_encoding=UTF8", "statement_timeout=5000", "sslmode=disable", "host=127.0.0.1", "user=root"}
	for _, pair := range reserved {
		if _, err := parseSessionVars("pg", []string{pair}); err == nil {
			t.Errorf("pg --session-var %q must be rejected as tool/driver-owned", pair)
		}
	}
	allowed := []string{"work_mem=64MB", "application_name=imex", "search_path=app", "timezone=UTC", "synchronous_commit=off"}
	vars, err := parseSessionVars("pg", allowed)
	if err != nil {
		t.Fatalf("pg must accept ordinary GUC names, got %v", err)
	}
	// Values arrive SQL-literal-quoted, the same form MySQL hands to its SET:
	// numbers and the keywords default/on/off/true/false stay bare, the rest is
	// single-quoted. pgSessionVarApply turns these into "SET <name> = <value>".
	if vars["work_mem"] != "'64MB'" || vars["synchronous_commit"] != "off" || vars["application_name"] != "'imex'" {
		t.Errorf("pg session-var values mis-quoted: %+v", vars)
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
		{"import_modes", "import", append(append([]string{}, base...), "-i", "/tmp/x.sql", "-t", "rt", "-f", "sql", "--mode", "replace")},
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
		{"dm_default", "dm", "", "", 5236},
		{"dm_explicit_port", "dm", "6000", "", 6000},
		{"dm_env_port", "dm", "", "6100", 6100},
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
		if f.Usage != "Database engine: mysql, pg, dm" {
			t.Errorf("%s --engine usage = %q", name, f.Usage)
		}
	}
}

// TestDMSurfaceRefusals pins the two commands whose Dameng scope is a subset of
// the tool's: `list` serves four catalog objects and `show` serves none. The
// refusals are the CLI face of the same table the import and export paths read,
// so an unmeasured object exits 2 without dialling.
func TestDMSurfaceRefusals(t *testing.T) {
	dmList := func(object string) *config.ListConfig {
		cfg := config.NewListConfig()
		cfg.Engine = "dm"
		cfg.Host = "127.0.0.1"
		cfg.Port = 5236
		cfg.User = "dmprobe"
		cfg.Password = "pw"
		cfg.Database = "DMPROBE"
		cfg.Object = object
		return cfg
	}

	const dmListReason = "this object has not been measured against Dameng Oracle-mode catalog; supported objects are databases, tables, columns and indexes"
	served := map[string]bool{
		"databases": true, "tables": true, "columns": true, "indexes": true,
	}
	for _, object := range list.Objects() {
		cfg := dmList(object)
		if object == "columns" || object == "indexes" {
			cfg.Table = "RT"
		}
		if object == "tables" {
			cfg.Type = "all"
		}
		err := validateListConfig(cfg)
		if served[object] {
			if err != nil {
				t.Errorf("list %s on dm must validate: %v", object, err)
			}
			continue
		}
		want := "list " + object + " is not supported with --engine dm: " + dmListReason
		if err == nil || err.Error() != want {
			t.Errorf("list %s refusal = %v, want %q", object, err, want)
		}
		if !common.IsUsageError(err) {
			t.Errorf("list %s refusal must exit 2", object)
		}
	}

	const dmShowEvtReason = "Dameng Oracle mode has no event scheduler, so there is no event definition to show; scheduled work lives in the DM job package"
	for _, kind := range show.Kinds() {
		err := checkShowEngineOptions(backend.EngineDM, kind.Short)
		if kind.Short == "evt" {
			want := "show evt is not supported with --engine dm: " + dmShowEvtReason
			if err == nil || err.Error() != want {
				t.Errorf("show evt refusal = %v, want %q", err, want)
			}
			if !common.IsUsageError(err) {
				t.Errorf("show evt refusal must exit 2")
			}
			continue
		}
		if err != nil {
			t.Errorf("show %s on dm must be supported, got refusal %v", kind.Short, err)
		}
	}

	// A hand-built config with a spelling the flag parser accepts must hit the
	// same wall, so the refusal does not depend on how --engine was written.
	cfg := dmList("events")
	cfg.Engine = " Dameng "
	if err := validateListConfig(cfg); err == nil || !strings.Contains(err.Error(), "list events is not supported with --engine dm") {
		t.Errorf(`--engine " Dameng " must normalise to dm and refuse events, got %v`, err)
	}
}

// TestDMOptionsAreAccepted is the other half of the Dameng matrix: what the first
// cut does serve. Anything dropped from the refusal table has to show up here, so
// an acceptance is a deliberate claim rather than a deleted row.
func TestDMOptionsAreAccepted(t *testing.T) {
	cases := []struct {
		name string
		sub  string
		args []string
	}{
		{"export_del", "export", dmExportFlags()},
		{"export_parquet", "export", dmExportFlags("-f", "parquet", "-o", "/tmp/imex-engine-test.parquet")},
		{"export_threads_one", "export", dmExportFlags("--threads", "1")},
		{"export_hex_blob", "export", dmExportFlags("--hex-blob")},
		{"import_insert", "import", dmImportFlags()},
		{"import_replace", "import", dmImportFlags("--mode", "replace")},
		{"import_parquet", "import", dmImportFlags("-f", "parquet", "-i", "/tmp/imex-engine-test.parquet")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateOnly(t, c.sub, c.args); err != nil {
				t.Errorf("dm %s refused %v: %v", c.sub, c.args, err)
			}
		})
	}

	// The charset vocabulary Dameng keeps is MySQL's, so utf8mb4 is accepted and a
	// name neither engine knows is refused with the engine-level wording.
	cfg := dmListConfigForCharsetTest()
	cfg.DefaultCharset = "utf8mb4"
	if err := validateListConfig(cfg); err != nil {
		t.Errorf("dm must accept utf8mb4: %v", err)
	}
	cfg.DefaultCharset = "nonsense"
	err := validateListConfig(cfg)
	if err == nil || !strings.HasPrefix(err.Error(), `invalid --default-charset "nonsense"`) {
		t.Errorf("dm charset refusal = %v", err)
	}
}

func dmListConfigForCharsetTest() *config.ListConfig {
	cfg := config.NewListConfig()
	cfg.Engine = "dm"
	cfg.Host = "127.0.0.1"
	cfg.Port = 5236
	cfg.User = "dmprobe"
	cfg.Password = "pw"
	cfg.Object = "databases"
	return cfg
}
