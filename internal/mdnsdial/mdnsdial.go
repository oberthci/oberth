// Package mdnsdial dials addresses whose host is an mDNS ".local" name. The
// released binaries are static (cgo off), so Go's own resolver answers from
// /etc/hosts and DNS only and never asks nss-mdns (avahi), while ssh and git
// on the same machine resolve the name through glibc (#066). On Linux a
// failed lookup of a .local name falls back to `getent ahosts`, which goes
// through the system's name service switch.
package mdnsdial

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Hint says why a .local name did not resolve and what to do instead.
const Hint = "mDNS .local names need avahi/nss-mdns; use the IP or add it to /etc/hosts"

// IsLocalName reports whether host is an mDNS name (ends in ".local").
func IsLocalName(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return strings.HasSuffix(host, ".local") && len(host) > len(".local")
}

// Lookup resolves a .local name to IP addresses in preference order; tests
// replace it.
var Lookup = getentLookup

// interfaceAddrs lists this machine's interface networks; tests replace it.
var interfaceAddrs = localNetworks

// hostnameRegexp is a DNS name safe to pass to getent as one argument: no
// leading "-", no spaces, no shell metacharacters (none is used anyway).
var hostnameRegexp = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*\.?$`)

// getentLookup asks the system resolver through `getent ahosts`. It returns
// nothing off Linux: other platforms' Go resolvers already reach mDNS.
func getentLookup(ctx context.Context, name string) ([]net.IP, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("no system resolver fallback on " + runtime.GOOS)
	}
	if len(name) > 253 || !hostnameRegexp.MatchString(name) {
		return nil, errors.New("not a hostname")
	}
	path, err := exec.LookPath("getent")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "ahosts", name).Output() // #nosec G204 -- fixed binary, validated hostname, no shell.
	if err != nil {
		return nil, err
	}
	return preferLocalNetworks(parseAhosts(out), interfaceAddrs()), nil
}

// parseAhosts reads the first column of `getent ahosts` output: one address
// per socket type, so each address appears up to three times.
func parseAhosts(out []byte) []net.IP {
	var ips []net.IP
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		ip := net.ParseIP(fields[0])
		if ip == nil || slices.ContainsFunc(ips, ip.Equal) {
			continue
		}
		ips = append(ips, ip)
	}
	return ips
}

// bridgePrefixes name container and pod bridges; avahi advertises their
// addresses too (k3s's cni0 is 10.42.0.1), and no other machine reaches them.
var bridgePrefixes = []string{"cni", "flannel", "docker", "br-", "veth", "cali", "virbr", "vxlan", "kube"}

// localNetworks are the networks of this machine's up, non-bridge interfaces.
func localNetworks() []*net.IPNet {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var nets []*net.IPNet
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || slices.ContainsFunc(bridgePrefixes, func(p string) bool { return strings.HasPrefix(iface.Name, p) }) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				nets = append(nets, ipNet)
			}
		}
	}
	return nets
}

// preferLocalNetworks moves addresses on one of nets first, keeping the
// resolver's order otherwise.
func preferLocalNetworks(ips []net.IP, nets []*net.IPNet) []net.IP {
	onNet := func(ip net.IP) bool {
		return slices.ContainsFunc(nets, func(n *net.IPNet) bool { return n.Contains(ip) })
	}
	var first, rest []net.IP
	for _, ip := range ips {
		if onNet(ip) {
			first = append(first, ip)
		} else {
			rest = append(rest, ip)
		}
	}
	return append(first, rest...)
}

// Dialer dials like Base and, when Base cannot resolve a .local host, dials
// the addresses Lookup returns in order.
type Dialer struct {
	Base *net.Dialer
}

// DialContext is a net.Dialer.DialContext replacement.
func (d Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	base := d.Base
	if base == nil {
		base = &net.Dialer{}
	}
	conn, err := base.DialContext(ctx, network, address)
	var dnsErr *net.DNSError
	if err == nil || !errors.As(err, &dnsErr) {
		return conn, err
	}
	host, port, splitErr := net.SplitHostPort(address)
	if splitErr != nil || !IsLocalName(host) {
		return nil, err
	}
	ips, lookupErr := Lookup(ctx, host)
	if lookupErr != nil || len(ips) == 0 {
		return nil, err // the original DNS error: callers explain it with Hint
	}
	var lastErr error
	for _, ip := range ips {
		conn, lastErr = base.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if lastErr == nil {
			return conn, nil
		}
	}
	return nil, lastErr
}

// IsUnresolvedLocal reports whether err is a failed lookup of a .local name.
func IsUnresolvedLocal(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && IsLocalName(dnsErr.Name)
}
