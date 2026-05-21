package repository

import (
	"net"
	"strings"
)

// parseIPv6 normalizes a stringified IP to an IPv6 representation
// (IPv4 is encoded as IPv4-mapped IPv6). Empty / invalid input → ::.
func parseIPv6(s string) net.IP {
	s = strings.TrimSpace(s)
	if s == "" {
		return net.IPv6zero
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return net.IPv6zero
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.To16()
	}
	return ip
}
