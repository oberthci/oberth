package secretstore

import (
	"crypto/x509"
	"net"
)

// ConstrainSelfSignedServerCertificate prepares the template of Oberth's
// self-signed server certificate to be its own trust anchor.
//
// Clients pin that certificate directly: the CLI and MCP clients through
// OBERTH_CA_CERT / NODE_EXTRA_CA_CERTS, pipeline pods through the goproxy CA.
// BoringSSL-based clients (Claude Code's runtime among them) accept a
// self-signed certificate as an anchor only when it may sign certificates, so
// a plain CA:FALSE leaf can never be trusted there.
//
// Being a CA is otherwise a liability: a leaked server key could mint
// certificates for any host that every pinning client would accept. The
// critical name constraints limit what it could ever vouch for to its own
// DNS names and exact IP addresses, and MaxPathLen 0 forbids intermediates.
func ConstrainSelfSignedServerCertificate(cert *x509.Certificate) {
	cert.BasicConstraintsValid = true
	cert.IsCA = true
	cert.MaxPathLen = 0
	cert.MaxPathLenZero = true
	cert.KeyUsage |= x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign
	cert.PermittedDNSDomainsCritical = true
	cert.PermittedDNSDomains = append([]string(nil), cert.DNSNames...)
	cert.PermittedIPRanges = nil
	for _, ip := range cert.IPAddresses {
		bits := 8 * net.IPv6len
		if ip4 := ip.To4(); ip4 != nil {
			ip, bits = ip4, 8*net.IPv4len
		}
		cert.PermittedIPRanges = append(cert.PermittedIPRanges, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
}
