package releaseverify

import (
	"net/url"
	"path"
	"slices"
	"strings"
)

// Origin is the public base for release retrieval: the download address of
// the project's GitHub releases, so a file is Origin/<tag>/<name>.
const Origin = "https://github.com/oberthci/oberth/releases/download"

// AssetHosts are the hosts GitHub serves release asset bytes from after a
// redirect; GitHub's domain list for self-hosted runners names both for
// release downloads. The fetcher carries a generated copy.
const AssetHosts = "release-assets.githubusercontent.com objects.githubusercontent.com"

// MaxRedirects bounds the redirects one release download follows.
const MaxRedirects = 3

// RedirectAllowed reports whether a release download may follow a redirect to
// target: over HTTPS, either back under Origin or to one of AssetHosts. Trust
// does not depend on it; every file is still checked against the pinned key.
func RedirectAllowed(target *url.URL) bool {
	if target.Scheme != "https" || target.User != nil {
		return false
	}
	if slices.Contains(strings.Fields(AssetHosts), target.Host) {
		return true
	}
	origin, err := url.Parse(Origin)
	return err == nil && target.Host == origin.Host && strings.HasPrefix(path.Clean(target.Path), origin.Path+"/")
}
