package backend

// MySQL's side of the option checks is empty, and that is a statement about the
// engine rather than an unfinished port: every object `imex list` enumerates, every
// format it writes, every mode it imports with and every kind it shows are defined
// in this engine's grammar, and `--session-var` is literally its own
// `SET <name>=<value>`. A rule table with no rows is the honest form of "nothing to
// refuse", and it is what makes the cmd-side loop engine-blind.
//
// The one MySQL-specific thing in the chain is not a refusal but a value: the
// 16-name --default-charset vocabulary in charset_mysql.go (KnownCharset). SET
// NAMES takes the server's own spelling, so the accepted set is the engine's, not
// cmd's.
func (mysqlBackend) Refusals(Command) []Refusal { return nil }
