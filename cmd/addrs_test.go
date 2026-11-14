package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/imex/internal/config"
)

func TestResolveConnectionAddrs(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		port     int
		portSpec string
		want     []config.Addr
		wantHost string
		wantPort int
	}{
		{
			name: "single address is a no-op", host: "127.0.0.1", port: 3306,
			want: nil, wantHost: "127.0.0.1", wantPort: 3306,
		},
		{
			name: "single address with an explicit port", host: "db.internal", port: 3307,
			portSpec: "3307", want: nil, wantHost: "db.internal", wantPort: 3307,
		},
		{
			name: "one port is broadcast to every host", host: "1.1.1.1,2.2.2.2", port: 3306,
			portSpec: "3306",
			want:     []config.Addr{{Host: "1.1.1.1", Port: 3306}, {Host: "2.2.2.2", Port: 3306}},
			wantHost: "1.1.1.1", wantPort: 3306,
		},
		{
			name: "N ports pair one-to-one with N hosts", host: "1.1.1.1,2.2.2.2", port: 3306,
			portSpec: "3306,3307",
			want:     []config.Addr{{Host: "1.1.1.1", Port: 3306}, {Host: "2.2.2.2", Port: 3307}},
			wantHost: "1.1.1.1", wantPort: 3306,
		},
		{
			name: "whitespace around entries is ignored", host: " 1.1.1.1 , 2.2.2.2 ", port: 3306,
			portSpec: " 3306 , 3307 ",
			want:     []config.Addr{{Host: "1.1.1.1", Port: 3306}, {Host: "2.2.2.2", Port: 3307}},
			wantHost: "1.1.1.1", wantPort: 3306,
		},
		{
			name: "an empty PortSpec falls back to Config.Port", host: "1.1.1.1,2.2.2.2", port: 5432,
			want:     []config.Addr{{Host: "1.1.1.1", Port: 5432}, {Host: "2.2.2.2", Port: 5432}},
			wantHost: "1.1.1.1", wantPort: 5432,
		},
		{
			name: "three hosts with three ports", host: "a,b,c", port: 1,
			portSpec: "1,2,3",
			want:     []config.Addr{{Host: "a", Port: 1}, {Host: "b", Port: 2}, {Host: "c", Port: 3}},
			wantHost: "a", wantPort: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Host: tc.host, Port: tc.port, PortSpec: tc.portSpec}
			if err := resolveConnectionAddrs(cfg); err != nil {
				t.Fatalf("resolveConnectionAddrs: %v", err)
			}
			if len(cfg.Addrs) != len(tc.want) {
				t.Fatalf("Addrs = %v, want %v", cfg.Addrs, tc.want)
			}
			for i := range tc.want {
				if cfg.Addrs[i] != tc.want[i] {
					t.Errorf("Addrs[%d] = %+v, want %+v", i, cfg.Addrs[i], tc.want[i])
				}
			}
			if cfg.Host != tc.wantHost || cfg.Port != tc.wantPort {
				t.Errorf("Host/Port = %q/%d, want %q/%d", cfg.Host, cfg.Port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

func TestResolveConnectionAddrsRejects(t *testing.T) {
	cases := []struct {
		name, host, portSpec string
		port                 int
		want                 string
	}{
		{"empty host", "", "", 3306, "--host is required"},
		{"empty host in list", "a,,b", "3306", 3306, "empty address in the list"},
		{"port count mismatch", "a,b", "3306,3307,3308", 3306, "expected 1 or 2 ports for 2 hosts, got 3"},
		{"one host but two ports", "a", "3306,3307", 3306, "expected 1 or 1 ports for 1 hosts, got 2"},
		{"non-numeric port", "a,b", "3306,abc", 3306, `invalid --port "abc"`},
		{"empty port entry", "a,b", "3306,", 3306, `invalid --port ""`},
		{"out of range port keeps the old message", "a", "70000", 3306, "invalid --port 70000: must be between 1 and 65535"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Host: tc.host, Port: tc.port, PortSpec: tc.portSpec}
			err := resolveConnectionAddrs(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestResolveConnectionAddrsIsIdempotent(t *testing.T) {
	cfg := &config.Config{Host: "1.1.1.1,2.2.2.2", Port: 3306, PortSpec: "3306,3307"}
	if err := resolveConnectionAddrs(cfg); err != nil {
		t.Fatal(err)
	}
	first := append([]config.Addr(nil), cfg.Addrs...)
	if err := resolveConnectionAddrs(cfg); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(cfg.Addrs) != len(first) || cfg.Addrs[0] != first[0] || cfg.Addrs[1] != first[1] {
		t.Fatalf("second pass degraded the list: %v, want %v", cfg.Addrs, first)
	}
}

func TestPortFlagKeepsItsIntHelpShape(t *testing.T) {
	f := exportCmd.Flags().Lookup("port")
	if f == nil {
		t.Fatal("--port is not registered")
	}
	if f.Value.Type() != "int" {
		t.Errorf("Type() = %q, want int (the help column must not change)", f.Value.Type())
	}
	if f.DefValue != "3306" {
		t.Errorf("DefValue = %q, want 3306", f.DefValue)
	}
}

func TestPortSpecValueKeepsConfigPortInStep(t *testing.T) {
	cfg := &config.Config{Port: 3306}
	v := portSpecValue{cfg}
	if err := v.Set("3307"); err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 3307 || cfg.PortSpec != "3307" {
		t.Errorf("Port/PortSpec = %d/%q, want 3307/3307", cfg.Port, cfg.PortSpec)
	}
	if err := v.Set("3306,3307"); err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 3306 || cfg.PortSpec != "3306,3307" {
		t.Errorf("Port/PortSpec = %d/%q, want 3306/3306,3307", cfg.Port, cfg.PortSpec)
	}
}

func TestMultiAddressEnvironmentVariables(t *testing.T) {
	t.Setenv("IMEX_HOST", "1.1.1.1,2.2.2.2")
	t.Setenv("IMEX_PORT", "3306,3307")
	f := pflag.NewFlagSet("env", pflag.ContinueOnError)
	cfg := &config.Config{Host: "127.0.0.1", Port: 3306}
	if _, err := applyConnEnv(f, cfg); err != nil {
		t.Fatalf("applyConnEnv: %v", err)
	}
	if err := resolveConnectionAddrs(cfg); err != nil {
		t.Fatalf("resolveConnectionAddrs: %v", err)
	}
	want := []config.Addr{{Host: "1.1.1.1", Port: 3306}, {Host: "2.2.2.2", Port: 3307}}
	if len(cfg.Addrs) != 2 || cfg.Addrs[0] != want[0] || cfg.Addrs[1] != want[1] {
		t.Fatalf("Addrs = %v, want %v", cfg.Addrs, want)
	}
}

func TestSingleValueIMEXPortErrorIsUnchanged(t *testing.T) {
	t.Setenv("IMEX_PORT", "nope")
	f := pflag.NewFlagSet("env", pflag.ContinueOnError)
	cfg := &config.Config{Port: 3306}
	_, err := applyConnEnv(f, cfg)
	if err == nil || !strings.Contains(err.Error(), `invalid IMEX_PORT "nope": must be a port number between 1 and 65535`) {
		t.Fatalf("err = %v, want the original IMEX_PORT message", err)
	}
	if cfg.Port != 3306 {
		t.Errorf("a rejected IMEX_PORT must not change Port, got %d", cfg.Port)
	}
}

func TestMultiAddressFlagsThroughValidation(t *testing.T) {
	base := []string{"-h", "1.1.1.1,2.2.2.2", "-u", "root", "-p", "pass", "-D", "mydb",
		"-o", "/tmp/x.del", "-q", "SELECT 1"}
	if err := validateOnly(t, "export", append(base, "-P", "3306,3307")); err != nil {
		t.Fatalf("validateOnly: %v", err)
	}
	want := []config.Addr{{Host: "1.1.1.1", Port: 3306}, {Host: "2.2.2.2", Port: 3307}}
	if len(exportCfg.Addrs) != 2 || exportCfg.Addrs[0] != want[0] || exportCfg.Addrs[1] != want[1] {
		t.Fatalf("Addrs = %v, want %v", exportCfg.Addrs, want)
	}
	if exportCfg.Host != "1.1.1.1" || exportCfg.Port != 3306 {
		t.Errorf("Host/Port = %q/%d, want 1.1.1.1/3306", exportCfg.Host, exportCfg.Port)
	}
}

func TestMultiAddressKeepsTheEngineDefaultPort(t *testing.T) {
	// --engine pg with neither -P nor IMEX_PORT must still broadcast 5432,
	// not the 3306 the flag's help default names.
	args := []string{"--engine", "pg", "-h", "1.1.1.1,2.2.2.2", "-u", "pg", "-p", "pw",
		"-D", "mydb", "-o", "/tmp/x.del", "-q", "SELECT 1"}
	if err := validateOnly(t, "export", args); err != nil {
		t.Fatalf("validateOnly: %v", err)
	}
	want := []config.Addr{{Host: "1.1.1.1", Port: 5432}, {Host: "2.2.2.2", Port: 5432}}
	if len(exportCfg.Addrs) != 2 || exportCfg.Addrs[0] != want[0] || exportCfg.Addrs[1] != want[1] {
		t.Fatalf("Addrs = %v, want %v", exportCfg.Addrs, want)
	}
}

func TestMultiAddressPortMismatchIsAUsageError(t *testing.T) {
	base := []string{"-h", "1.1.1.1,2.2.2.2", "-u", "root", "-p", "pass", "-D", "mydb",
		"-o", "/tmp/x.del", "-q", "SELECT 1"}
	err := validateOnly(t, "export", append(base, "-P", "3306,3307,3308"))
	if err == nil || !strings.Contains(err.Error(), "expected 1 or 2 ports for 2 hosts, got 3") {
		t.Fatalf("err = %v, want the port-count usage error", err)
	}
}
