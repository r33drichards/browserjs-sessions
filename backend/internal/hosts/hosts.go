// Package hosts compares the host names requests arrive with.
package hosts

import (
	"net"
	"strings"
)

// Split takes a request's Host (or any "name" or "name:port") apart. The
// name comes back in lower case and without a trailing dot, so two spellings
// of one host compare equal; port is "" when there is none.
func Split(host string) (name, port string) {
	name = host
	if n, p, err := net.SplitHostPort(host); err == nil {
		name, port = n, p
	}
	return strings.TrimSuffix(strings.ToLower(name), "."), port
}
