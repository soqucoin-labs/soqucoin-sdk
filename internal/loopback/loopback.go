// Package loopback answers one question for both network clients: does a
// host, as written, name this machine's loopback interface. The rpc client
// refuses a password-bearing request to any other host until it is told the
// host is remote on purpose; the electrumx client refuses a plaintext
// connection to any other host until it is told the path is private. One
// definition, so the two refusals cannot drift apart.
package loopback

import (
	"net"
	"strings"
)

// Host reports whether host is the name "localhost" or an IP literal the
// standard library classifies as loopback (127.0.0.0/8, ::1). Nothing is
// resolved: a name that resolves to this machine is not loopback, and neither
// is a spelling such as "127.1" that is not a valid literal. Both refusals
// read the string the operator wrote, so a copied or mistyped host fails
// instead of being reached.
func Host(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
