package main

import (
	"net/url"
	"path"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/reponame"
)

// parseRepositoryAddress accepts the forge's own SSH user and path. The user
// is kept unless it is the default one, and the path is held in one clean
// absolute spelling, so equal addresses compare equal.
func parseRepositoryAddress(address string) (*url.URL, error) {
	u, err := url.Parse(repositoryAddressURL(address))
	if err != nil {
		return nil, invalidRepositoryAddress()
	}
	if u.User != nil {
		_, password := u.User.Password()
		if password || u.Scheme != "ssh" {
			return nil, publicerr.New(publicerr.InvalidInput, "Repository URLs must not contain credentials.", "Remove the username and password from the forge URL and retry.", nil)
		}
	}
	if u.RawQuery != "" || u.ForceQuery || strings.Contains(address, "#") {
		return nil, publicerr.New(publicerr.InvalidInput, "Repository URLs must not contain a query or fragment.", "Remove the query and fragment from the forge URL and retry.", nil)
	}
	if u.Hostname() == "" || strings.Trim(u.Path, "/") == "" {
		return nil, invalidRepositoryAddress()
	}
	if err = reponame.ValidateForgeURL(u); err != nil {
		return nil, err
	}
	if reponame.ForgeUser(u) == reponame.DefaultForgeUser {
		u.User = nil
	}
	u.Path = "/" + strings.Trim(u.Path, "/")
	return u, nil
}

// upstreamAddress is a forge address without its repository: the account and
// namespace a new upstream reaches. The namespace's last part is the
// organization in the repository's catalogue name.
func upstreamAddress(u *url.URL) string {
	base := *u
	base.Path = path.Dir(u.Path)
	return base.String()
}

func repositoryAddressURL(address string) string {
	if strings.Contains(address, "://") {
		return address
	}
	start := strings.LastIndex(address, "@") + 1
	delimiter := ":"
	if strings.HasPrefix(address[start:], "[") {
		delimiter = "]:"
	}
	offset := strings.Index(address[start:], delimiter)
	if offset < 0 {
		return address
	}
	separator := start + offset + len(delimiter) - 1
	if strings.Contains(address[:separator], "/") {
		return address
	}
	return "ssh://" + address[:separator] + "/" + address[separator+1:]
}

func invalidRepositoryAddress() error {
	return publicerr.New(publicerr.InvalidInput, "Repository address is invalid.", "Use ssh://user@host/path/repository.git or user@host:path/repository.git.", nil)
}
