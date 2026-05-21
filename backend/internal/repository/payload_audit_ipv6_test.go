package repository

import (
	"net"
	"testing"
)

func TestParseIPv6(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "::"},
		{"   ", "::"},
		{"not an ip", "::"},
		{"127.0.0.1", "::ffff:127.0.0.1"},
		{"192.168.1.1", "::ffff:192.168.1.1"},
		{"2001:db8::1", "2001:db8::1"},
		{"::1", "::1"},
	}
	for _, c := range cases {
		got := parseIPv6(c.in)
		want := net.ParseIP(c.want)
		if !got.Equal(want) {
			t.Errorf("parseIPv6(%q) = %v, want %v", c.in, got, want)
		}
	}
}
