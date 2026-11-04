package backend

// This file holds the vocabulary of the --engine option checks, not the checks
// themselves. Those live in cmd, which owns the command line; what belongs to a
// dialect is the answer to "which of these options can this engine not honour, and
// what do we tell the operator why".
//
// Refusals are data rather than methods per option for two reasons. The first is
// order: the checks have always reported in a fixed sequence (the output format
// first, because it decides which writer the rest of the options belong to), and
// the sequence a message is reported in is part of the exit-text contract, so it
// has to travel with the rules instead of being re-derived by each caller. The
// second is that `backend` cannot import cobra: two of the rules fire on whether an
// option was *named* rather than on its value, so cmd folds that fact into
// RunOptions and the rule only ever sees a bool.

// Command names the CLI surface a run is on. The four engines checks are per
// command because the same flag means different things on different ones:
// --threads 4 is refused for export and left alone for import, and --mode belongs
// to import only.
type Command int

const (
	CmdExport Command = iota
	CmdImport
	CmdList
	CmdShow
)

// RunOptions is everything a refusal rule may look at, folded by cmd out of the
// command's own config and flag state.
//
// The fields are the tool's vocabulary, not a server's: Format, Mode and Object are
// the values the operator typed, Threads and NoHexBlob are already resolved, and
// SessionVar says whether --session-var was named, which is the only case where the
// presence of an option is the problem rather than its value. ShowKind is empty
// outside `imex show`.
type RunOptions struct {
	Format     string
	Mode       string
	Object     string
	ShowKind   string
	Threads    int
	NoHexBlob  bool
	SessionVar bool
}

// Refusal is one option the engine cannot serve.
//
// Option renders the left-hand side of the message exactly as the operator has read
// it so far — usually a flag spelling, sometimes the command and the object or kind
// it was asked about — which is why it is a function of the run rather than a
// constant: `imex show table` and `imex show view` report different labels out of
// one rule. Reason is the text after the colon, and it names the alternative rather
// than restating the refusal. Applies decides from the folded options.
type Refusal struct {
	Option  func(o RunOptions) string
	Reason  string
	Applies func(o RunOptions) bool
}

// static is the common case of Option: the rule names one flag, spelled the way the
// operator spelled it.
func static(option string) func(RunOptions) string {
	return func(RunOptions) string { return option }
}
