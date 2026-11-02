package config

// ShowConfig holds the options of one `imex show <kind> <name>` run.
//
// The embedded Config carries the connection options, whose Database field is
// the schema the object is looked up in when the positional name carries no
// qualifier of its own. Schema and Object are the resolved pair the command
// queries, filled in by RunE after the name has been split.
//
// Output and NoOverwrite follow `imex export`: the definition goes to a file
// when --output names one, and without it the result goes to stdout, or to the
// --messages file when one was given (docs/requirements.md 3.6).
type ShowConfig struct {
	Config

	Ref    string // the positional name as typed, kept for the configuration record
	Schema string // resolved qualifier ("" when the object is unqualified and the connection has no schema)
	Object string // resolved object name

	Output      string // --output file path, empty when the result has no file
	NoOverwrite bool   // refuse to truncate an existing --output file
}

// NewShowConfig returns a ShowConfig with the shipped defaults.
func NewShowConfig() *ShowConfig {
	return &ShowConfig{Config: defaultConfig()}
}
