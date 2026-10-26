package cmd

import (
	"fmt"
	"github.com/pkg/errors"
	"os"

	"github.com/imex/internal/common"
	"github.com/spf13/cobra"
)

var encryptCmd = &cobra.Command{
	Use:   "encrypt [password]",
	Short: "Encrypt password for secure automation",
	Long: `Encrypt a password using AES-256-GCM encryption.

The encrypted password can be safely used in command line arguments
and scripts without exposing the plaintext password in process listings.

The encryption key must be provided via IMEX_KEY environment variable.

Examples:
  # Encrypt a password
  export IMEX_KEY="your-secret-key"
  imex encrypt "my-password"
  # Output: ENC:base64-encoded-encrypted-data...

  # Use encrypted password in export
  imex export -h 127.0.0.1 -u root -p "ENC:base64..." -D mydb -o backup.sql`,
	Args: exactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		password := args[0]

		key := os.Getenv(common.EnvKey)
		if key == "" {
			return errors.Errorf("environment variable %s is not set\n"+
				"Please set it first: export %s=\"your-secret-key\"",
				common.EnvKey, common.EnvKey)
		}

		encrypted, err := common.EncryptPassword(password, key)
		if err != nil {
			return errors.Wrap(err, "failed to encrypt password")
		}

		fmt.Println(encrypted)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(encryptCmd)
}
