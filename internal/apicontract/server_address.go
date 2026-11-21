package apicontract

import (
	"errors"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// ParseServerAddress normalizes the MCP endpoint path and refuses credentials
// and any query or fragment delimiter.
func ParseServerAddress(address string) (*url.URL, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	if u.Hostname() == "" || u.User != nil || u.Opaque != "" || strings.ContainsAny(address, "?#") {
		return nil, errors.New("invalid server address")
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	if !strings.HasSuffix(path, "/mcp") {
		path += "/mcp"
	}
	u.Path, _ = url.PathUnescape(path)
	u.RawPath = path
	return u, nil
}

// ASCIIHost reports whether a nonempty host contains only ASCII characters.
func ASCIIHost(host string) bool {
	return host != "" && !strings.ContainsFunc(host, func(r rune) bool { return r > 127 })
}

// PunycodeAddress converts a parsed address's host without changing its port or path.
func PunycodeAddress(address *url.URL) (string, error) {
	host, err := idna.Lookup.ToASCII(address.Hostname())
	if err != nil {
		return "", err
	}
	ascii := *address
	ascii.Host = strings.Replace(address.Host, address.Hostname(), host, 1)
	return ascii.String(), nil
}

// SameServerHost compares nonempty ASCII hosts without folding Unicode names.
func SameServerHost(a, b string) bool {
	return ASCIIHost(a) && ASCIIHost(b) && strings.EqualFold(a, b)
}

// SameServerAddress compares scheme, ASCII host, port and escaped path.
// An omitted HTTPS port is 443; base and MCP endpoint spellings compare equally.
func SameServerAddress(a, b string) bool {
	left, e1 := ParseServerAddress(a)
	right, e2 := ParseServerAddress(b)
	if e1 != nil || e2 != nil || !SameServerHost(left.Hostname(), right.Hostname()) {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && serverPort(left) == serverPort(right) && left.EscapedPath() == right.EscapedPath()
}

func serverPort(u *url.URL) string {
	if u.Port() == "" && strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return u.Port()
}
