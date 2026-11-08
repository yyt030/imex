package config

// ExecConfig holds the options of one `imex exec` run.
//
// The embedded Config carries the connection options, whose Database field is
// the schema the statements run in (the backend's connection string selects it
// the same way export, import, list and show select it). Unlike those commands,
// exec does not require it: a statement may carry its own schema qualifier or
// land in the connection's default.
//
// The SQL text itself is not stored here. It is read from --file, a positional
// argument, or stdin after validation, and handed straight to internal/exec.Run,
// so keeping a copy in the config would be a second source of truth. File records
// only where it came from, for the configuration echo.
//
// Output and NoOverwrite follow `imex show`: the result goes to a file when
// --output names one, and otherwise to stdout, or to the --messages file when one
// was given (docs/requirements.md 3.6). NoColNames drops the header row of a
// result set, mirroring `imex list`.
type ExecConfig struct {
	Config

	File string // --file path the SQL came from, empty for a positional argument or stdin

	Output      string // --output file path, empty when the result has no file
	NoOverwrite bool   // refuse to truncate an existing --output file
	NoColNames  bool   // do not print the column-header row of a result set
}

// NewExecConfig returns an ExecConfig with the shipped defaults.
func NewExecConfig() *ExecConfig {
	return &ExecConfig{Config: defaultConfig()}
}
