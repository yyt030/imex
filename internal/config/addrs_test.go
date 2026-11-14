package config

import "testing"

func TestFormatAddrs(t *testing.T) {
	cases := []struct {
		name  string
		addrs []Addr
		want  string
	}{
		{"nil renders nothing", nil, ""},
		{"empty renders nothing", []Addr{}, ""},
		{"one endpoint", []Addr{{Host: "127.0.0.1", Port: 3306}}, "127.0.0.1:3306"},
		{"two endpoints keep their order", []Addr{{Host: "1.1.1.1", Port: 3306}, {Host: "2.2.2.2", Port: 3307}}, "1.1.1.1:3306,2.2.2.2:3307"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatAddrs(tc.addrs); got != tc.want {
				t.Errorf("FormatAddrs = %q, want %q", got, tc.want)
			}
		})
	}
}
