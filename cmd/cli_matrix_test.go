package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset global config to defaults
			*exportCfg = *newExportConfigForTest()
			*importCfg = *newImportConfigForTest()
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
