package cmd

import (
	"strconv"
	"strings"

	"github.com/spf13/pflag"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// This file is the CLI half of multi-address support. The other half is
// backend.OpenWithFailover, which turns the resolved list into one connection
// per dial attempt.
//
// --host has always been a string, so a comma-separated list fits it without a
// shape change. --port was an int: registering portSpecValue instead keeps the
// rendered help line exactly as it was (`-P, --port int ... (default 3306)`,
// no quotes around the number) while letting Set receive "3306,3307". The
// Type() string is "int" for that help column only; the value is parsed below,
// not by pflag, so a bad entry is reported by the CLI's own usage error rather
// than by strconv inside the flag parser.

// portSpecValue is the --port flag value. It stores the raw text so that the
// comma-separated case survives unchanged into resolveConnectionAddrs, and
// also keeps Config.Port pointing at the first port: that field is what the
// required-parameter checks, the config record and callers that build a
// Config by hand read, so a typed -P 3307 must be visible there immediately,
// not only after validation.
type portSpecValue struct{ cfg *config.Config }

func (v portSpecValue) Set(s string) error {
	v.cfg.PortSpec = s
	first := s
	if i := strings.IndexByte(s, ','); i >= 0 {
		first = s[:i]
	}
	if n, err := strconv.Atoi(strings.TrimSpace(first)); err == nil {
		v.cfg.Port = n
	}
	return nil
}

func (v portSpecValue) String() string {
	if v.cfg.PortSpec == "" {
		return "3306"
	}
	return v.cfg.PortSpec
}

// Type reports "int" so the help column and the unquoted default stay as they
// were when --port was an IntVarP. It is presentation only: no numeric parsing
// happens in pflag for this flag.
func (v portSpecValue) Type() string { return "int" }

var _ pflag.Value = portSpecValue{}

func splitAddrList(s string) []string {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func parsePortList(spec string) ([]int, error) {
	items := splitAddrList(spec)
	if len(items) == 0 {
		return nil, common.Usagef("invalid --port %q: must be a port number between 1 and 65535", spec)
	}
	out := make([]int, 0, len(items))
	for _, s := range items {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, common.Usagef("invalid --port %q: must be a port number between 1 and 65535", s)
		}
		out = append(out, n)
	}
	// The range check is deliberately left to validatePort below: a single
	// out-of-range port then reports the exact same message as before
	// ("invalid --port 70000: must be between 1 and 65535").
	return out, nil
}

// resolveConnectionAddrs folds --host / --port (or IMEX_HOST / IMEX_PORT) into
// the endpoint list. The two shapes the option accepts:
//
//	-h 1.1.1.1,2.2.2.2 -P 3306        -> 1.1.1.1:3306 and 2.2.2.2:3306
//	-h 1.1.1.1,2.2.2.2 -P 3306,3307  -> 1.1.1.1:3306 and 2.2.2.2:3307
//
// One port is broadcast to every host; N ports must match N hosts one-to-one;
// anything else is a usage error. A single address resolves to a nil Addrs,
// which is the switch that keeps every downstream path byte-for-byte identical
// to the pre-multi-address tool. Host and Port are set to the first endpoint,
// so the `export config` / `import config` records still name one host/port
// and the existing required-parameter checks still see a non-empty host.
//
// It is idempotent: a Config that already carries a resolved list is left
// alone, because validation can be reached more than once by a library caller.
func resolveConnectionAddrs(cfg *config.Config) error {
	if len(cfg.Addrs) > 1 {
		return nil
	}

	hosts := splitAddrList(cfg.Host)
	if len(hosts) == 0 || hosts[0] == "" {
		return common.Usagef("--host is required")
	}
	for _, h := range hosts {
		if h == "" {
			return common.Usagef("invalid --host %q: empty address in the list", cfg.Host)
		}
	}

	var ports []int
	if cfg.PortSpec == "" {
		ports = []int{cfg.Port}
	} else {
		var err error
		ports, err = parsePortList(cfg.PortSpec)
		if err != nil {
			return err
		}
	}

	switch {
	case len(ports) == 1:
		for len(ports) < len(hosts) {
			ports = append(ports, ports[0])
		}
	case len(ports) != len(hosts):
		return common.Usagef("invalid --port %q: expected 1 or %d ports for %d hosts, got %d",
			cfg.PortSpec, len(hosts), len(hosts), len(ports))
	}
	for _, p := range ports {
		if err := validatePort(p); err != nil {
			return err
		}
	}

	cfg.Host = hosts[0]
	cfg.Port = ports[0]
	if len(hosts) == 1 {
		cfg.Addrs = nil
		return nil
	}
	cfg.Addrs = make([]config.Addr, len(hosts))
	for i := range hosts {
		cfg.Addrs[i] = config.Addr{Host: hosts[i], Port: ports[i]}
	}
	return nil
}
