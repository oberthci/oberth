package client

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"net/url"
	"strconv"
	"strings"
)

// ValidateSSHEndpoint admits the installation endpoint carried by enrollment and profiles.
func ValidateSSHEndpoint(endpoint, serverURL string) (*url.URL, error) {
	// Leave ample room for the rest of the bounded installation profile.
	if len(endpoint) > 4096 || validateAddress(serverURL) != nil {
		return nil, failure("invitation", nil)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, failure("invitation", nil)
	}
	server, _ := url.Parse(serverURL)
	if !validSSHShape(u, server) || strings.HasSuffix(u.Host, ":") {
		return nil, failure("invitation", nil)
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, failure("invitation", err)
		}
	}
	return u, nil
}

func validSSHShape(u, server *url.URL) bool {
	return u.Scheme == "ssh" && apicontract.SameServerHost(u.Hostname(), server.Hostname()) && u.User != nil && u.User.String() == "git" && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawFragment == "" && u.Opaque == ""
}
