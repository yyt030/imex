package cmd

import (
	"context"

	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
	"github.com/gomydump/internal/export"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

var exportCfg = config.NewExportConfig()

var exportCmd = &cobra.Command{
	Use:     "export",
	Aliases: []string{"exp"},
	Short:   "Export data from MySQL/OceanBase",
	Long: `Export data from a MySQL or OceanBase database to a DEL (delimited ASCII),
SQL, or Parquet format file.

Examples:
  # Basic export to DEL format
  gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
    -o /backup/users.del -q "SELECT * FROM users"

  # Export to SQL format
  gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
    -o /backup/users.sql -f sql -q "SELECT * FROM users"

  # Export to Parquet format
  gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
    -o /backup/users.parquet -f parquet -q "SELECT * FROM users"

  # Parallel export with 4 threads
  gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
    -o /backup/users.del --threads 4 -q "SELECT * FROM users"

  # Custom delimiters
  gomydump export -h 127.0.0.1 -u root -p pass -D mydb \
    -o /backup/users.del --char-del '!' --col-del '@' \
    -q "SELECT * FROM users"`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Apply --default-charset override if explicitly provided.
		if exportDefaultCharset != "" {
			exportCfg.DefaultCharset = exportDefaultCharset
		}

		// Required connection parameters.
		if exportCfg.Host == "" && exportCfg.Socket == "" {
			return common.Usagef("--host or --socket is required")
		}
		if exportCfg.User == "" {
			return common.Usagef("--user is required")
		}
		if exportCfg.Password == "" {
			return common.Usagef("password is required")
		}
		if exportCfg.Database == "" {
			return common.Usagef("--database is required")
		}
		if exportCfg.Output == "" {
			return common.Usagef("--output is required")
		}
		if exportCfg.Query == "" {
			return common.Usagef("--query is required")
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

		// Parse session variables.
		vars, err := common.ParseSessionVars(exportCfg.SessionVars)
		if err != nil {
			return err
		}
		exportCfg.Vars = vars
		if len(vars) > 0 {
			common.Logger.Info("Session variables will be set on every connection", "vars", exportCfg.SessionVars)
		}

		common.Logger.Info("export command started",
			"output", exportCfg.Output,
			"format", exportCfg.Format,
			"host", exportCfg.Host,
			"port", exportCfg.Port,
			"database", exportCfg.Database,
		)

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
