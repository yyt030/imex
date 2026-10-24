package cmd

import (
	"context"

	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
	_import "github.com/gomydump/internal/import"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

var importCfg = config.NewImportConfig()

var importCmd = &cobra.Command{
	Use:     "import",
	Aliases: []string{"imp"},
	Args:    cobra.NoArgs,
	Short:   "Import data into MySQL/OceanBase",
	Long: `Import data from a DEL (delimited ASCII), SQL, or Parquet format file into a
MySQL or OceanBase database.

Examples:
  # Basic import from DEL format
  gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
    -i /backup/users.del -t users

  # Import from Parquet format
  gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
    -i /backup/users.parquet -f parquet -t users

  # Import with insert_update mode
  gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
    -i /backup/users.del --mode insert_update -t users

  # Create table from SQL file
  gomydump import -h 127.0.0.1 -u root -p pass -D mydb \
    -i /backup/users.sql -f sql --mode create`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Apply --default-charset override if explicitly provided.
		if importDefaultCharset != "" {
			importCfg.DefaultCharset = importDefaultCharset
		}

		// Required connection parameters.
		if importCfg.Host == "" && importCfg.Socket == "" {
			return common.Usagef("--host or --socket is required")
		}
		if importCfg.User == "" {
			return common.Usagef("--user is required")
		}
		if importCfg.Password == "" {
			return common.Usagef("password is required")
		}
		if importCfg.Database == "" {
			return common.Usagef("--database is required")
		}
		if importCfg.Input == "" {
			return common.Usagef("--input is required")
		}
		if importCfg.Mode != config.ImportModeCreate && importCfg.Table == "" {
			return common.Usagef("--table is required (except for --mode create)")
		}

		// Resolve password securely.
		password, err := common.ResolvePassword(importCfg.Password)
		if err != nil {
			return errors.Wrap(err, "failed to resolve password")
		}
		importCfg.Password = password

		// Parse delimiters.
		if err := parseImportDelimiters(); err != nil {
			return err
		}

		// Validate all parameters (order: threads → batch-size → pipeline-buffer → rest).
		if err := validateImportConfig(cmd, importCfg); err != nil {
			return err
		}

		// Init logger with user's chosen level.
		common.InitLogger(importCfg.LogLevel)

		// Parse session variables.
		vars, err := common.ParseSessionVars(importCfg.SessionVars)
		if err != nil {
			// Malformed --session-var values are parameter errors: report them
			// as usage errors so they exit 2 like every other validation.
			return common.Usagef("%v", err)
		}
		importCfg.Vars = vars
		if len(vars) > 0 {
			common.Logger.Info("session variables will be set on every connection", "vars", importCfg.SessionVars)
		}

		importer, err := _import.NewImporter(importCfg)
		if err != nil {
			return errors.Wrap(err, "failed to create importer")
		}
		defer importer.Close()

		ctx := context.Background()
		if err := importer.Import(ctx); err != nil {
			return errors.Wrap(err, "import failed")
		}

		return nil
	},
}

func init() {
	registerImportFlags(importCmd)
}

// parseImportDelimiters parses DEL format delimiter strings.
func parseImportDelimiters() error {
	var err error
	if importCfg.DelModifier.CharDel, err = common.ParseDelimiter(importCfg.DelModifier.CharDel); err != nil {
		return common.Usagef("invalid --char-del: %v", err)
	}
	if importCfg.DelModifier.ColDel, err = common.ParseDelimiter(importCfg.DelModifier.ColDel); err != nil {
		return common.Usagef("invalid --col-del: %v", err)
	}
	if importCfg.DelModifier.RowDel, err = common.ParseDelimiter(importCfg.DelModifier.RowDel); err != nil {
		return common.Usagef("invalid --row-del: %v", err)
	}
	if importCfg.DelModifier.Escape, err = common.ParseDelimiter(importCfg.DelModifier.Escape); err != nil {
		return common.Usagef("invalid --escape: %v", err)
	}
	return nil
}
