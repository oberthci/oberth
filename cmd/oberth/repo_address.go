package main

import (
	"net/url"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
)

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
		if u.User.Username() != "git" {
			return nil, publicerr.New(publicerr.InvalidInput, "Oberth reaches forges as git@.", "Use git@host in the SSH forge URL and retry.", nil)
		}
		u.User = nil
	}
	if u.RawQuery != "" || u.ForceQuery || strings.Contains(address, "#") {
		return nil, publicerr.New(publicerr.InvalidInput, "Repository URLs must not contain a query or fragment.", "Remove the query and fragment from the forge URL and retry.", nil)
	}
	if u.Hostname() == "" || strings.Trim(u.Path, "/") == "" {
		return nil, invalidRepositoryAddress()
	}
	return u, nil
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
	return publicerr.New(publicerr.InvalidInput, "Repository address is invalid.", "Use ssh://host/organization/repository.git or git@host:organization/repository.git.", nil)
}
