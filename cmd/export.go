package cmd

import (
	"context"
	"os"
	"strings"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
	"github.com/imex/internal/export"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

var exportCfg = config.NewExportConfig()

var exportCmd = &cobra.Command{
	Use:     "export",
	Aliases: []string{"exp"},
	Args:    cobra.NoArgs,
	Short:   "Export data from MySQL/OceanBase",
	Long: `Export data from a MySQL or OceanBase database to a DEL (delimited ASCII),
SQL, or Parquet format file.

Examples:
  # Basic export to DEL format
  imex export -h 127.0.0.1 -u root -p pass -D mydb \
    -o /backup/users.del -q "SELECT * FROM users"

  # Export to SQL format
  imex export -h 127.0.0.1 -u root -p pass -D mydb \
    -o /backup/users.sql -f sql -q "SELECT * FROM users"

  # SQL export is data-only; add --create when the dump must be self-contained
  # (import ... --mode create reads the CREATE TABLE from the file)
  imex export -h 127.0.0.1 -u root -p pass -D mydb \
    -o /backup/users.sql -f sql --create -q "SELECT * FROM users"

  # Read the query from a file instead of the command line
  imex export -h 127.0.0.1 -u root -p pass -D mydb \
    -o /backup/users.sql -f sql -q @/backup/users_query.sql

  # Export to Parquet format
  imex export -h 127.0.0.1 -u root -p pass -D mydb \
    -o /backup/users.parquet -f parquet -q "SELECT * FROM users"

  # Parallel export with 4 threads
  imex export -h 127.0.0.1 -u root -p pass -D mydb \
    -o /backup/users.del --threads 4 -q "SELECT * FROM users"

  # Custom delimiters
  imex export -h 127.0.0.1 -u root -p pass -D mydb \
    -o /backup/users.del --char-del '!' --col-del '@' \
    -q "SELECT * FROM users"`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// Apply --default-charset override if explicitly provided.
		if exportDefaultCharset != "" {
			exportCfg.DefaultCharset = exportDefaultCharset
		}

		// Fill the connection fields the run left unset from the environment
		// (IMEX_HOST, IMEX_PORT, IMEX_USER, IMEX_PASSWORD, IMEX_SOCKET,
		// IMEX_DATABASE). This happens before the required-parameter checks so an
		// environment-supplied database satisfies them exactly like a typed -D.
		connEnv, err := applyConnEnv(cmd.Flags(), &exportCfg.Config)
		if err != nil {
			return err
		}

		// Required connection parameters.
		if exportCfg.Host == "" && exportCfg.Socket == "" {
			return common.Usagef("--host or --socket is required")
		}
		if exportCfg.User == "" {
			return common.Usagef("--user is required")
		}
		if exportCfg.Password == "" {
			return common.Usagef("--password is required (or set IMEX_PASSWORD)")
		}
		if exportCfg.Database == "" {
			return common.Usagef("--database is required")
		}
		if exportCfg.Output == "" {
			return common.Usagef("--output is required")
		}
		if err := resolveExportQuery(); err != nil {
			return err
		}

		// Resolve password securely.
		password, err := common.ResolvePassword(exportCfg.Password)
		if err != nil {
			return errors.Wrap(err, "failed to resolve password")
		}
		exportCfg.Password = password

		// Parse delimiters.
		if err := parseExportDelimiters(); err != nil {
			return err
		}

		// Validate all parameters (order: threads → batch-size → pipeline-buffer → rest).
		if err := validateExportConfig(cmd, exportCfg); err != nil {
			return err
		}

		// Init logger with user's chosen level.
		common.InitLogger(exportCfg.LogLevel)
		logConnEnv(connEnv)

		// Parse session variables.
		vars, err := common.ParseSessionVars(exportCfg.SessionVars)
		if err != nil {
			// Malformed --session-var values are parameter errors: report them
			// as usage errors so they exit 2 like every other validation.
			return common.Usagef("%v", err)
		}
		exportCfg.Vars = vars
		if len(vars) > 0 {
			common.Logger.Info("session variables will be set on every connection", "vars", exportCfg.SessionVars)
		}

		exporter, err := export.NewExporter(exportCfg)
		if err != nil {
			return errors.Wrap(err, "failed to create exporter")
		}
		defer exporter.Close()

		ctx := context.Background()
		if err := exporter.Export(ctx); err != nil {
			return errors.Wrap(err, "export failed")
		}

		return nil
	},
}

func init() {
	registerExportFlags(exportCmd)
}

// resolveExportQuery turns the -q value into the single effective query. A
// value starting with "@" is a file name: its contents become the statement,
// with surrounding whitespace and a single trailing semicolon trimmed (a query
// file is normally written for a SQL client). "@@" escapes a literal leading
// "@", so a statement that really starts with an at-sign can still be passed
// inline. An empty value, an unreadable file, and a file with no statement all
// fail here, before any connection is opened.
func resolveExportQuery() error {
	if exportCfg.Query == "" {
		return common.Usagef("-q is required (use -q @file to read it from a file)")
	}
	if strings.HasPrefix(exportCfg.Query, "@@") {
		exportCfg.Query = exportCfg.Query[1:]
		return nil
	}
	if !strings.HasPrefix(exportCfg.Query, "@") {
		return nil
	}
	path := exportCfg.Query[1:]
	if path == "" {
		return common.Usagef("invalid -q: missing file path after @")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return common.Usagef("cannot read query file %q: %v", path, err)
	}
	sqlText := strings.TrimSpace(string(data))
	sqlText = strings.TrimRight(sqlText, ";")
	sqlText = strings.TrimSpace(sqlText)
	if sqlText == "" {
		return common.Usagef("query file %q contains no SQL statement", path)
	}
	exportCfg.Query = sqlText
	exportCfg.QueryFile = path
	return nil
}

// parseExportDelimiters parses DEL format delimiter strings.
func parseExportDelimiters() error {
	var err error
	if exportCfg.DelModifier.CharDel, err = common.ParseDelimiter(exportCfg.DelModifier.CharDel); err != nil {
		return common.Usagef("invalid --char-del: %v", err)
	}
	if exportCfg.DelModifier.ColDel, err = common.ParseDelimiter(exportCfg.DelModifier.ColDel); err != nil {
		return common.Usagef("invalid --col-del: %v", err)
	}
	if exportCfg.DelModifier.RowDel, err = common.ParseDelimiter(exportCfg.DelModifier.RowDel); err != nil {
		return common.Usagef("invalid --row-del: %v", err)
	}
	if exportCfg.DelModifier.Escape, err = common.ParseDelimiter(exportCfg.DelModifier.Escape); err != nil {
		return common.Usagef("invalid --escape: %v", err)
	}
	return nil
}
