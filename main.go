package main

import (
	"os"
	"strings"

	"github.com/gomydump/cmd"
	"github.com/gomydump/internal/common"
)

func main() {
	if err := cmd.Execute(); err != nil {
		// Usage errors (parameter validation) exit with code 2.
		// Cobra's "unknown flag" errors are also usage errors.
		if common.IsUsageError(err) || strings.Contains(err.Error(), "unknown flag") {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
