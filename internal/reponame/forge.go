package reponame

import (
	"net/url"
	"strings"
)

// DefaultForgeUser is the SSH account a forge address logs in as when it
// names none.
const DefaultForgeUser = "git"

// HomePrefix opens a path relative to the SSH user's home in Git's ssh://
// form, ssh://user@host/~/path.
const HomePrefix = "~/"

// ForgeUser is the SSH account a forge address logs in as.
func ForgeUser(u *url.URL) string {
	if u.User == nil {
		return DefaultForgeUser
	}
	return u.User.Username()
}

// ValidateForgeURL accepts the SSH user and the path a forge address names.
// Neither can begin a command-line option, carry a control or shell character,
// or leave its directory through "..", so an upstream reaches exactly the
// account and namespace it was registered with. Only the first path part may
// be ~, the SSH user's home.
func ValidateForgeURL(u *url.URL) error {
	if u.User != nil {
		_, password := u.User.Password()
		if password || !validForgeUser(u.User.Username()) {
			return &InvalidNameError{Component: "forge user", Value: u.User.Username()}
		}
	}
	if u.RawPath != "" {
		return &InvalidNameError{Component: "forge path", Value: u.EscapedPath()}
	}
	trimmed := strings.Trim(u.Path, "/")
	if trimmed == "" {
		return nil
	}
	// A leading ~ is Git's home of the SSH user; nowhere else is it a name.
	trimmed = strings.TrimPrefix(trimmed, HomePrefix)
	for _, part := range strings.Split(trimmed, "/") {
		if !validSegment(part) {
			return &InvalidNameError{Component: "forge path", Value: u.Path}
		}
	}
	return nil
}

func validForgeUser(value string) bool {
	return len(value) <= 32 && validSegment(value)
}
