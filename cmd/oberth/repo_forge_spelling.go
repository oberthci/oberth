package main

import (
	"net/url"
	"strings"
)

// sameForgeSpelling selects diagnostic wording; it never admits a repository.
func sameForgeSpelling(base string, supplied *url.URL) bool {
	stored, err := url.Parse(base)
	return err == nil && stored.Scheme == supplied.Scheme && stored.Host != supplied.Host &&
		forgeDiagnosticHost(stored) == forgeDiagnosticHost(supplied)
}

func forgeDiagnosticHost(u *url.URL) string {
	host := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, u.Hostname())
	port := u.Port()
	if u.Scheme == "ssh" && port == "22" || u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" || u.Scheme == "git" && port == "9418" {
		port = ""
	}
	return strings.TrimSuffix(host, ".") + "|" + port
}
