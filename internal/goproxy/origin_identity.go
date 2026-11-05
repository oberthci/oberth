package goproxy

import (
	"github.com/oberthci/oberth/internal/gitcache"
)

// publicOriginURL separates repository identity from fetch transport. Only a
// standard Codeberg SSH remote matching the already-qualified route has a known
// public identity. Compare exact spellings to exclude credentials, escapes and
// ambiguous paths; never resolve a mutable remote or catalog here. The caller
// still validates the snapshot's SSH transport before constructing metadata.
func publicOriginURL(repository, remote string) string {
	upstream, owner, repo, err := gitcache.ParseRepoPath(repository)
	if err != nil || upstream == "" || owner == "" || repository != upstream+"/"+owner+"/"+repo {
		return remote
	}
	suffix := "/" + owner + "/" + repo + ".git"
	for _, host := range []string{"codeberg.org", "codeberg.org:22"} {
		if remote == "ssh://git@"+host+suffix {
			return "https://codeberg.org" + suffix
		}
	}
	return remote
}
