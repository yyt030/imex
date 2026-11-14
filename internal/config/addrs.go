package config

import (
	"strconv"
	"strings"
)

// Addr is one database endpoint. --host and --port accept comma-separated
// lists; cmd's resolveConnectionAddrs folds the two into this list. The field
// is deliberately additive: a Config with a nil or length-1 Addrs is the
// single-address case, and every downstream path (DSN builders, the config
// record, the issued SQL) stays byte-for-byte what it was before multi-address
// support. Host and Port keep their old meaning on that path.
//
// The backend never iterates this slice itself. backend.OpenWithFailover picks
// one Addr per connection, copies it into a single-address Config and hands
// that to the engine's ordinary Open, so no dialect had to learn a second
// address shape.
type Addr struct {
	Host string
	Port int
}

// FormatAddrs renders an endpoint list for the configuration record. Both the
// export and import records need the same spelling, and a nil/empty list
// renders as the empty string so callers can use it as "print nothing".
func FormatAddrs(addrs []Addr) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, a.Host+":"+strconv.Itoa(a.Port))
	}
	return strings.Join(parts, ",")
}
