package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
)

// TestCLIMatrix runs a matrix of valid/invalid flag combinations.
// This test exercises the validation layer without requiring a real database.
func TestCLIMatrix(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantErr   bool
		errSubstr string
	}{
		// Valid combinations
		{
			name:    "valid export minimal",
			args:    []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1"},
			wantErr: false,
		},
		{
			name:    "valid export with batch-size 1 and pipeline-buffer 0",
			args:    []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "--batch-size", "1", "--pipeline-buffer", "0"},
			wantErr: false,
		},
		{
			name:    "valid export with statement-size 100 and format sql",
			args:    []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.sql", "-q", "SELECT 1", "-f", "sql", "--statement-size", "100"},
			wantErr: false,
		},
		{
			name:    "valid export with single-row-insert and format sql",
			args:    []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.sql", "-q", "SELECT 1", "-f", "sql", "--single-row-insert"},
			wantErr: false,
		},
		{
			name:    "valid import minimal",
			args:    []string{"import", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-i", "/tmp/in.del", "-t", "users"},
			wantErr: false,
		},

		// Numeric boundary violations
		{
			name:      "batch-size 0 rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "--batch-size", "0"},
			wantErr:   true,
			errSubstr: "--batch-size",
		},
		{
			name:      "batch-size 100001 rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "--batch-size", "100001"},
			wantErr:   true,
			errSubstr: "--batch-size",
		},
		{
			name:      "pipeline-buffer -1 rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "--pipeline-buffer", "-1"},
			wantErr:   true,
			errSubstr: "--pipeline-buffer",
		},
		{
			name:      "pipeline-buffer 1025 rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "--pipeline-buffer", "1025"},
			wantErr:   true,
			errSubstr: "--pipeline-buffer",
		},
		{
			name:      "statement-size 0 rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.sql", "-q", "SELECT 1", "-f", "sql", "--statement-size", "0"},
			wantErr:   true,
			errSubstr: "--statement-size",
		},
		{
			name:      "statement-size 268435457 rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.sql", "-q", "SELECT 1", "-f", "sql", "--statement-size", "268435457"},
			wantErr:   true,
			errSubstr: "--statement-size",
		},
		{
			name:      "threads 0 rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "--threads", "0"},
			wantErr:   true,
			errSubstr: "--threads",
		},
		{
			name:      "threads 257 rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "--threads", "257"},
			wantErr:   true,
			errSubstr: "--threads",
		},

		// Deleted options should be rejected as unknown
		{
			name:      "ssl-mode rejected as unknown",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "--ssl-mode", "disabled"},
			wantErr:   true,
			errSubstr: "unknown flag",
		},
		{
			name:      "tenant rejected as unknown",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "--tenant", "mytenant"},
			wantErr:   true,
			errSubstr: "unknown flag",
		},
		{
			name:      "codepage rejected as unknown",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "--codepage", "utf8mb4"},
			wantErr:   true,
			errSubstr: "unknown flag",
		},
		{
			name:      "dec-pt rejected as unknown",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "--dec-pt", "."},
			wantErr:   true,
			errSubstr: "unknown flag",
		},
		{
			name:      "strip-lzeros rejected as unknown",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "--strip-lzeros"},
			wantErr:   true,
			errSubstr: "unknown flag",
		},

		// Format-specific option validation
		{
			name:      "format del with statement-size rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "-f", "del", "--statement-size", "100"},
			wantErr:   true,
			errSubstr: "--statement-size can only be used with --format sql",
		},
		{
			name:      "format del with single-row-insert rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "-f", "del", "--single-row-insert"},
			wantErr:   true,
			errSubstr: "--single-row-insert can only be used with --format sql",
		},
		{
			name:    "valid export with extended-insert and format sql",
			args:    []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.sql", "-q", "SELECT 1", "-f", "sql", "--extended-insert"},
			wantErr: false,
		},
		{
			name:      "format del with extended-insert rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "-f", "del", "--extended-insert"},
			wantErr:   true,
			errSubstr: "--extended-insert can only be used with --format sql",
		},
		{
			name:      "single-row-insert with extended-insert rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.sql", "-q", "SELECT 1", "-f", "sql", "--single-row-insert", "--extended-insert"},
			wantErr:   true,
			errSubstr: "cannot be used together",
		},
		{
			name:      "format parquet with single-row-insert rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.parquet", "-q", "SELECT 1", "-f", "parquet", "--single-row-insert"},
			wantErr:   true,
			errSubstr: "--single-row-insert can only be used with --format sql",
		},
		{
			name:      "mode create with format del rejected",
			args:      []string{"import", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-i", "/tmp/in.del", "-t", "users", "--mode", "create", "-f", "del"},
			wantErr:   true,
			errSubstr: "--mode create can only be used with --format sql",
		},
		{
			name:      "global log-format bogus rejected",
			args:      []string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.del", "-q", "SELECT 1", "--log-format", "bogus"},
			wantErr:   true,
			errSubstr: "invalid --log-format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset global config to defaults
			*exportCfg = *newExportConfigForTest()
			*importCfg = *newImportConfigForTest()
			// logFormat is a package-level flag target: pflag only writes it when
			// the flag is given, so a case passing --log-format bogus would leave
			// that value in place for every following case.
			logFormat = "java"
			// cobra.Cmd.Flags().Changed() is per-Flag state and the command tree
			// is package-level, so a case that sets --statement-size would
			// otherwise still look "changed" to the next case.
			resetChangedFlags(exportCmd, importCmd, rootCmd)

			// Set up root command
			rootCmd.SetArgs(tt.args)

			// Capture error
			err := rootCmd.Execute()

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errSubstr)
				} else if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("expected error containing %q, got %q", tt.errSubstr, err.Error())
				}
			} else {
				// For valid cases, we expect the command to fail at the database connection stage,
				// not at validation. So we check that the error is NOT a validation error.
				if err != nil && strings.Contains(err.Error(), "invalid --") {
					t.Errorf("unexpected validation error: %v", err)
				}
			}
		})
	}
}

// validateOnly parses args on the export/import command and runs just the
// validation layer. Going through rootCmd.Execute() would open a database
// connection (and retry it) for every combination that validation accepts.
func validateOnly(t *testing.T, sub string, args []string) error {
	t.Helper()
	*exportCfg = *newExportConfigForTest()
	*importCfg = *newImportConfigForTest()
	logFormat = "java"
	resetChangedFlags(exportCmd, importCmd, rootCmd)

	if sub == "export" {
		if err := exportCmd.Flags().Parse(args); err != nil {
			return err
		}
		return validateExportConfig(exportCmd, exportCfg)
	}
	if err := importCmd.Flags().Parse(args); err != nil {
		return err
	}
	return validateImportConfig(importCmd, importCfg)
}

// TestCLIDELFlagFormatValidation pins the symmetry with the SQL-only options:
// every DEL modifier must be accepted for --format del and rejected as a usage
// error for the other two formats, on both subcommands. Silently ignoring them
// is what let a user believe the file was laid out differently than it is.
func TestCLIDELFlagFormatValidation(t *testing.T) {
	// One sample value per DEL modifier; bool flags take no value.
	delFlags := []struct {
		name  string
		value string
	}{
		{"--char-del", "'"},
		{"--col-del", "@"},
		{"--row-del", `\r\n`},
		{"--escape", "\\"},
		{"--no-escape", ""},
		{"--timestamp-fmt", "yyyy-MM-dd HH:mm"},
		{"--date-fmt", "dd/MM/yyyy"},
		{"--no-char-del", ""},
		{"--no-col-names", ""},
	}

	bases := map[string][]string{
		"export": {"-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.dat", "-q", "SELECT 1"},
		"import": {"-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-i", "/tmp/in.dat", "-t", "users"},
	}

	for _, sub := range []string{"export", "import"} {
		for _, f := range delFlags {
			for _, format := range []string{"del", "sql", "parquet"} {
				args := append([]string{}, bases[sub]...)
				args = append(args, "-f", format)
				if f.value == "" {
					args = append(args, f.name)
				} else {
					args = append(args, f.name, f.value)
				}

				t.Run(sub+" "+f.name+" with --format "+format, func(t *testing.T) {
					err := validateOnly(t, sub, args)
					if format == "del" {
						if err != nil {
							t.Fatalf("legal combination rejected: %v", err)
						}
						return
					}
					want := f.name + " can only be used with --format del"
					if err == nil {
						t.Fatalf("expected usage error %q, got nil", want)
					}
					if !strings.Contains(err.Error(), want) {
						t.Errorf("expected error containing %q, got %q", want, err.Error())
					}
					if !common.IsUsageError(err) {
						t.Errorf("error %q must be a usage error (exit code 2)", err)
					}
				})
			}
		}
	}
}

// TestCLIParquetHexBlobRejected covers --format parquet's other silent no-op:
// parquet stores binary columns natively, so the hex-blob switches cannot
// change the output and must not be accepted quietly.
func TestCLIParquetHexBlobRejected(t *testing.T) {
	base := []string{"-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb", "-o", "/tmp/out.dat", "-q", "SELECT 1"}

	for _, format := range []string{"del", "sql", "parquet"} {
		for _, flag := range []string{"--hex-blob", "--no-hex-blob"} {
			args := append([]string{}, base...)
			args = append(args, "-f", format, flag)

			t.Run("export "+flag+" with --format "+format, func(t *testing.T) {
				err := validateOnly(t, "export", args)
				if format != "parquet" {
					if err != nil {
						t.Fatalf("legal combination rejected: %v", err)
					}
					return
				}
				want := flag + " cannot be used with --format parquet"
				if err == nil {
					t.Fatalf("expected usage error %q, got nil", want)
				}
				if !strings.Contains(err.Error(), want) {
					t.Errorf("expected error containing %q, got %q", want, err.Error())
				}
				if !common.IsUsageError(err) {
					t.Errorf("error %q must be a usage error (exit code 2)", err)
				}
			})
		}
	}
}

// TestCLIDELFlagRejectedExitCode drives the rejection through the real command
// tree so the exit-2 contract is covered end to end, not just the validator.
func TestCLIDELFlagRejectedExitCode(t *testing.T) {
	*exportCfg = *newExportConfigForTest()
	*importCfg = *newImportConfigForTest()
	resetChangedFlags(exportCmd, importCmd, rootCmd)

	rootCmd.SetArgs([]string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb",
		"-o", "/tmp/out.sql", "-q", "SELECT 1", "-f", "sql", "--col-del", "@"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected usage error, got nil")
	}
	if !strings.Contains(err.Error(), "--col-del can only be used with --format del") {
		t.Errorf("unexpected error: %v", err)
	}
	if !common.IsUsageError(err) {
		t.Errorf("error %q must be a usage error (exit code 2)", err)
	}
}

// TestCLILogFormatEnvOverrideRejected covers the GOMYDUMP_LOG_FORMAT path: the
// env value goes through the same membership check as the flag, so a typo
// cannot silently fall back to the java layout.
func TestCLILogFormatEnvOverrideRejected(t *testing.T) {
	t.Setenv("GOMYDUMP_LOG_FORMAT", "javaness")
	*exportCfg = *newExportConfigForTest()
	*importCfg = *newImportConfigForTest()
	logFormat = "java"
	resetChangedFlags(exportCmd, importCmd, rootCmd)

	rootCmd.SetArgs([]string{"export", "-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb",
		"-o", "/tmp/out.del", "-q", "SELECT 1"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected usage error for GOMYDUMP_LOG_FORMAT=javaness, got nil")
	}
	if !strings.Contains(err.Error(), "invalid --log-format") {
		t.Errorf("unexpected error: %v", err)
	}
	if !common.IsUsageError(err) {
		t.Errorf("error %q must be a usage error (exit code 2)", err)
	}
}

// TestCLIInsertLayout pins the flipped SQL default: nothing given means one row
// per INSERT (ExtendedInsert false), --extended-insert opts back into merged
// statements, and the deprecated --single-row-insert parses without changing the
// default it now merely restates.
func TestCLIInsertLayout(t *testing.T) {
	base := []string{"-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb",
		"-o", "/tmp/out.sql", "-q", "SELECT 1", "-f", "sql"}

	if err := validateOnly(t, "export", base); err != nil {
		t.Fatalf("default layout: %v", err)
	}
	if exportCfg.ExtendedInsert {
		t.Error("default ExtendedInsert = true, one row per INSERT must be the default")
	}

	if err := validateOnly(t, "export", append(append([]string{}, base...), "--extended-insert")); err != nil {
		t.Fatalf("--extended-insert: %v", err)
	}
	if !exportCfg.ExtendedInsert {
		t.Error("--extended-insert did not reach the config")
	}

	if err := validateOnly(t, "export", append(append([]string{}, base...), "--single-row-insert")); err != nil {
		t.Fatalf("deprecated --single-row-insert must still parse: %v", err)
	}
	if exportCfg.ExtendedInsert {
		t.Error("--single-row-insert must not enable merged statements")
	}
}

// TestCLIProgressInterval pins the cadence option: absent flag keeps the
// shipped 5 s default on both subcommands, a positive value reaches the config
// verbatim, and a non-positive value is a usage error rather than a panic in
// time.NewTicker — an operator who writes 0 means "never", which is what
// --quiet already says, so the option does not take on that meaning too.
func TestCLIProgressInterval(t *testing.T) {
	base := []string{"-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb"}
	exportArgs := append(append([]string{}, base...), "-o", "/tmp/out.del", "-q", "SELECT 1")
	importArgs := append(append([]string{}, base...), "-i", "/tmp/in.del", "-t", "users")

	if err := validateOnly(t, "export", exportArgs); err != nil {
		t.Fatalf("export without --progress-interval: %v", err)
	}
	if exportCfg.ProgressInterval != config.DefaultProgressInterval {
		t.Errorf("export default = %v, want %v", exportCfg.ProgressInterval, config.DefaultProgressInterval)
	}
	if err := validateOnly(t, "import", importArgs); err != nil {
		t.Fatalf("import without --progress-interval: %v", err)
	}
	if importCfg.ProgressInterval != config.DefaultProgressInterval {
		t.Errorf("import default = %v, want %v", importCfg.ProgressInterval, config.DefaultProgressInterval)
	}

	for _, tc := range []struct {
		value   string
		want    time.Duration
		wantErr string
	}{
		{value: "1ms", want: time.Millisecond},
		{value: "30s", want: 30 * time.Second},
		{value: "0", wantErr: "invalid --progress-interval"},
		{value: "-1s", wantErr: "invalid --progress-interval"},
	} {
		flag := "--progress-interval=" + tc.value
		for _, sub := range []string{"export", "import"} {
			args := exportArgs
			if sub == "import" {
				args = importArgs
			}
			err := validateOnly(t, sub, append(append([]string{}, args...), flag))
			if tc.wantErr != "" {
				if err == nil {
					t.Errorf("%s %s: expected usage error, got nil", sub, flag)
					continue
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("%s %s: error %q lacks %q", sub, flag, err, tc.wantErr)
				}
				if !common.IsUsageError(err) {
					t.Errorf("%s %s: %q must be a usage error (exit code 2)", sub, flag, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s %s: unexpected error %v", sub, flag, err)
				continue
			}
			got := exportCfg.ProgressInterval
			if sub == "import" {
				got = importCfg.ProgressInterval
			}
			if got != tc.want {
				t.Errorf("%s %s: ProgressInterval = %v, want %v", sub, flag, got, tc.want)
			}
		}
	}
}

// TestCLIEncryptArgsCountIsUsageError pins the positional-argument check of the
// encrypt subcommand to the exit-code contract: a wrong argument count is a
// usage problem (exit code 2), the same as every rejected option. cobra's own
// ExactArgs returns a plain error, which main.go would map to exit code 1.
func TestCLIEncryptArgsCountIsUsageError(t *testing.T) {
	*exportCfg = *newExportConfigForTest()
	*importCfg = *newImportConfigForTest()
	logFormat = "java"
	resetChangedFlags(exportCmd, importCmd, rootCmd)
	defer resetChangedFlags(exportCmd, importCmd, rootCmd)

	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"missing password", []string{"encrypt"}, "encrypt accepts 1 argument(s), received 0"},
		{"extra password", []string{"encrypt", "a", "b"}, "encrypt accepts 1 argument(s), received 2"},
	} {
		rootCmd.SetArgs(tc.args)
		err := rootCmd.Execute()
		if err == nil {
			t.Fatalf("%s: expected usage error, got nil", tc.name)
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		}
		if !common.IsUsageError(err) {
			t.Errorf("%s: error %q must be a usage error (exit code 2)", tc.name, err)
		}
	}

	// A correct call must still pass the args check and reach RunE; with no key
	// set it fails as a runtime error, not as a usage error.
	t.Setenv("GOMYDUMP_KEY", "")
	rootCmd.SetArgs([]string{"encrypt", "secret"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("encrypt with empty GOMYDUMP_KEY should fail")
	}
	if common.IsUsageError(err) {
		t.Errorf("missing key is a runtime failure, not a usage error: %v", err)
	}
	if !strings.Contains(err.Error(), "GOMYDUMP_KEY") {
		t.Errorf("unexpected error: %v", err)
	}
}

// newExportConfigForTest creates a fresh ExportConfig with defaults.
func newExportConfigForTest() *config.ExportConfig {
	return config.NewExportConfig()
}

// resetChangedFlags clears the "was this flag set on the command line" marker of
// every flag that a previous case touched.
func resetChangedFlags(cmds ...*cobra.Command) {
	for _, c := range cmds {
		clear := func(f *pflag.Flag) { f.Changed = false }
		c.Flags().VisitAll(clear)
		c.PersistentFlags().VisitAll(clear)
	}
}

// newImportConfigForTest creates a fresh ImportConfig with defaults.
func newImportConfigForTest() *config.ImportConfig {
	return config.NewImportConfig()
}
