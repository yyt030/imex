package cmd

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/imex/internal/config"
)

// annotateOptionScopes appends every format-scoped flag's scope marker to its own
// help text, taken from the registry the validator enforces. `--help` therefore
// says exactly what a run rejects, and a new scoped option gets its help wording
// from the same entry that gives it its rejection message instead of from a
// hand-written string that can go stale.
//
// Idempotent, because the command tree is package-level and tests rebuild it.
func annotateOptionScopes(cmd *cobra.Command, scopes config.OptionScopes) {
	for _, sc := range scopes {
		flag := cmd.Flags().Lookup(sc.Flag)
		if flag == nil || strings.Contains(flag.Usage, sc.Hint) {
			continue
		}
		flag.Usage = flag.Usage + " " + sc.Hint
	}
}
