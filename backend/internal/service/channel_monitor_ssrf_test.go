package service

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLookupMonitorDialIPsPrefersIPv4(t *testing.T) {
	addrs := preferMonitorDialIPs([]net.IP{
		net.ParseIP("2606:4700:3035::6815:3050"),
		net.ParseIP("172.67.181.190"),
		net.ParseIP("104.21.48.80"),
	})

	require.Equal(t, []string{
		"172.67.181.190",
		"104.21.48.80",
		"2606:4700:3035::6815:3050",
	}, ipsToStrings(addrs))
}

func TestLookupMonitorDialIPsKeepsIPv6WhenNoIPv4Exists(t *testing.T) {
	addrs := preferMonitorDialIPs([]net.IP{
		net.ParseIP("2606:4700:3035::6815:3050"),
		net.ParseIP("2606:4700:3036::ac43:b5be"),
	})

	require.Equal(t, []string{
		"2606:4700:3035::6815:3050",
		"2606:4700:3036::ac43:b5be",
	}, ipsToStrings(addrs))
}

func ipsToStrings(addrs []net.IP) []string {
	out := make([]string, 0, len(addrs))
	for _, ip := range addrs {
		out = append(out, ip.String())
	}
	return out
}
