package backend

import "strings"

// gaussBackend drives the GaussDB family (GaussDB / openGauss).
//
// It speaks the PostgreSQL wire protocol and is reached through pgx exactly as
// postgresBackend is, so every connection, quoting, batching and import method
// is inherited unchanged. What differs — and the only reason this engine exists
// rather than being a flag on --engine pg — is `show`: a stock PostgreSQL server
// has no way to read back a whole table definition, but the Gauss family ships a
// native pg_get_tabledef(regclass-or-name) that reconstructs the CREATE TABLE.
//
// EXPERIMENTAL SPIKE, not a released engine: it is registered in backends and
// accepted by Parse, but is kept out of engineOrder so it never appears in the
// --engine help text or the oracle. On a real Gauss server pg_get_tabledef is
// built-in; a stock PostgreSQL does not have it, so `--engine gauss show table`
// against community PG only works once pg_get_tabledef has been defined in the
// database (this spike defines a local simulation for the end-to-end check).
type gaussBackend struct {
	postgresBackend
}

func (gaussBackend) Engine() Engine { return EngineGauss }

// gaussShowColumn is the result column a Gauss pg_get_tabledef call returns:
// a scalar function call names its single output column after the function.
const gaussShowColumn = "pg_get_tabledef"

// ShowDefinition prints a table via the native Gauss pg_get_tabledef.
//
// The reference is passed as a SQL text literal to the function (it casts to
// regclass internally), not as a quoted identifier, so a single quote in a name
// is doubled — the standard literal escape — to keep the statement inert against
// a crafted object name. pg_get_tabledef's text already ends in `;` (measured on
// the end-to-end server run), so hasTerminator is true and show must not append
// a second one. Other kinds (view/function/trigger/...) are not yet measured
// against a real Gauss server and stay refused here.
func (gaussBackend) ShowDefinition(object, schema, name string) (string, string, bool, bool) {
	if object != "table" {
		return "", "", false, false
	}
	ref := name
	if schema != "" {
		ref = schema + "." + name
	}
	ref = strings.ReplaceAll(ref, "'", "''")
	return "SELECT pg_get_tabledef('" + ref + "')", gaussShowColumn, true, true
}

// Refusals reuses the PostgreSQL rules for every command except `show`, where
// table definitions ARE printable through pg_get_tabledef and only the other
// kinds are refused.
//
// Note the kind arrives here as the CLI's SHORT name (cmd folds kind.Short into
// RunOptions.ShowKind), while show.Run later looks the definition up by the FULL
// word. Both spellings of the table kind are whitelisted so `show tab` and
// `show table` reach pg_get_tabledef; every other kind, and empty, is refused.
func (g gaussBackend) Refusals(c Command) []Refusal {
	if c != CmdShow {
		return g.postgresBackend.Refusals(c)
	}
	return []Refusal{
		{
			Option: func(o RunOptions) string { return "show " + o.ShowKind },
			Reason: "Gauss table definitions are read with pg_get_tabledef; this engine has not yet measured the other kinds, use pg_dump for those",
			Applies: func(o RunOptions) bool {
				return o.ShowKind != "" && o.ShowKind != "table" && o.ShowKind != "tab"
			},
		},
	}
}
