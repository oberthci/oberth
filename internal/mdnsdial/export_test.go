package mdnsdial

import (
	"errors"
	"net"
)

// IsUnresolvedLocal reports whether err is a failed lookup of a .local name.
func IsUnresolvedLocal(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && IsLocalName(dnsErr.Name)
}
