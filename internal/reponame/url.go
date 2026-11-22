package reponame

import (
	"net/url"
	"strings"
)

// URL builds a clone address using the upstream's stored namespace.
func (r Repository) URL(base string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	namespace := strings.TrimRight(u.Path, "/")
	if namespace == "" {
		namespace = "/" + r.Organization
	}
	u.Path, u.RawPath = namespace+"/"+r.Name+".git", ""
	return u, nil
}

// MatchesURL compares the stored scheme, SSH user, host, port and repository
// namespace.
func (r Repository) MatchesURL(base string, supplied *url.URL) bool {
	expected, err := r.URL(base)
	return err == nil && expected.Scheme == supplied.Scheme && ForgeUser(expected) == ForgeUser(supplied) && expected.Host == supplied.Host &&
		strings.TrimSuffix(expected.Path, ".git") == strings.TrimSuffix(strings.TrimRight(supplied.Path, "/"), ".git") &&
		supplied.RawQuery == "" && !supplied.ForceQuery && supplied.Fragment == ""
}
